// Package renter is responsible for uploading and downloading files on the sia
// network.
package renter

// CONCURRENCY PATTERNS: The renter has some complex concurrency patterns.
// Preventing race conditions and deadlocks requires understanding the patterns.
//
// The renter itself has a lock that protects all internal state. The renter is
// allowed to call out to the hostContractor while under lock, which means that
// calls within the hostContractor should not ever leave the hostContractor -
// external calls should be able to complete quickly, and without making any
// external calls or calls that may acquire external locks.
//
// The renter has a bunch of worker objects. The worker objects have mutexes
// which protect them, and the workers need to interact with the renter,
// sometimes changing state which is prevented by locks. This means that the
// renter itself can never interact with a worker while the renter is under
// lock.

// TODO: Change the upload loop to have an upload state, and make it so that
// instead of occasionally rebuilding the whole file matrix it has just a
// single matrix that it's constantly pulling chunks from. Have a separate loop
// which goes through the files and adds them to the matrix. Have the loop
// listen on the channel for new files, so that they can go directly into the
// matrix.

// TODO: Allow the 'baseMemory' to be set by the user.

import (
	"errors"
	"reflect"
	"sync"

	"github.com/pachisi456/Sia/build"
	"github.com/pachisi456/Sia/modules"
	"github.com/pachisi456/Sia/modules/renter/contractor"
	"github.com/pachisi456/Sia/modules/renter/hostdb"
	"github.com/pachisi456/Sia/persist"
	siasync "github.com/pachisi456/Sia/sync"
	"github.com/pachisi456/Sia/types"

	"github.com/NebulousLabs/threadgroup"
)

var (
	errNilContractor = errors.New("cannot create renter with nil contractor")
	errNilCS         = errors.New("cannot create renter with nil consensus set")
	errNilHdb        = errors.New("cannot create renter with nil hostdb")
	errNilTpool      = errors.New("cannot create renter with nil transaction pool")
)

var (
	// priceEstimationScope is the number of hosts that get queried by the
	// renter when providing price estimates. Especially for the 'Standard'
	// variable, there should be congruence with the number of contracts being
	// used in the renter allowance.
	priceEstimationScope = build.Select(build.Var{
		Standard: int(50),
		Dev:      int(12),
		Testing:  int(4),
	}).(int)
)

// A hostDB is a database of hosts that the renter can use for figuring out who
// to upload to, and download from.
type hostDB interface {
	// ActiveHosts returns the list of hosts that are actively being selected
	// from.
	ActiveHosts() []modules.HostDBEntry

	// AllHosts returns the full list of hosts known to the hostdb, sorted in
	// order of preference.
	AllHosts() []modules.HostDBEntry

	// AverageContractPrice returns the average contract price of a host.
	AverageContractPrice() types.Currency

	// Close closes the hostdb.
	Close() error

	// Host returns the HostDBEntry for a given host.
	Host(types.SiaPublicKey) (modules.HostDBEntry, bool)

	// RandomHosts returns a set of random hosts, weighted by their estimated
	// usefulness / attractiveness to the renter. RandomHosts will not return
	// any offline or inactive hosts.
	RandomHosts(int, []types.SiaPublicKey) []modules.HostDBEntry

	// ScoreBreakdown returns a detailed explanation of the various properties
	// of the host.
	ScoreBreakdown(modules.HostDBEntry) modules.HostScoreBreakdown

	// EstimateHostScore returns the estimated score breakdown of a host with the
	// provided settings.
	EstimateHostScore(modules.HostDBEntry) modules.HostScoreBreakdown
}

// A hostContractor negotiates, revises, renews, and provides access to file
// contracts.
type hostContractor interface {
	// SetAllowance sets the amount of money the contractor is allowed to
	// spend on contracts over a given time period, divided among the number
	// of hosts specified. Note that contractor can start forming contracts as
	// soon as SetAllowance is called; that is, it may block.
	SetAllowance(modules.Allowance) error

	// Allowance returns the current allowance
	Allowance() modules.Allowance

	// Close closes the hostContractor.
	Close() error

	// Contract returns the latest contract formed with the specified host.
	Contract(modules.NetAddress) (modules.RenterContract, bool)

	// Contracts returns the contracts formed by the contractor.
	Contracts() []modules.RenterContract

	// ContractByID returns the contract associated with the file contract id.
	ContractByID(types.FileContractID) (modules.RenterContract, bool)

	// CurrentPeriod returns the height at which the current allowance period
	// began.
	CurrentPeriod() types.BlockHeight

	// PeriodSpending returns the amount spent on contracts during the current
	// billing period.
	PeriodSpending() modules.ContractorSpending

	// Editor creates an Editor from the specified contract ID, allowing the
	// insertion, deletion, and modification of sectors.
	Editor(types.FileContractID, <-chan struct{}) (contractor.Editor, error)

	// IsOffline reports whether the specified host is considered offline.
	IsOffline(types.FileContractID) bool

	// Downloader creates a Downloader from the specified contract ID,
	// allowing the retrieval of sectors.
	Downloader(types.FileContractID, <-chan struct{}) (contractor.Downloader, error)

	// ResolveID returns the most recent renewal of the specified ID.
	ResolveID(types.FileContractID) types.FileContractID

	// ResovleContract returns the current contract associated with the provided
	// contract id. It is equivalent to calling 'ResolveID' and then using the
	// result to call 'ContractByID'.
	ResolveContract(types.FileContractID) (modules.RenterContract, bool)
}

// A trackedFile contains metadata about files being tracked by the Renter.
// Tracked files are actively repaired by the Renter. By default, files
// uploaded by the user are tracked, and files that are added (via loading a
// .sia file) are not.
type trackedFile struct {
	// location of original file on disk
	RepairPath string
}

// A Renter is responsible for tracking all of the files that a user has
// uploaded to Sia, as well as the locations and health of these files.
type Renter struct {
	// File management.
	//
	// tracking contains a list of files that the user intends to maintain. By
	// default, files loaded through sharing are not maintained by the user.
	files    map[string]*file
	tracking map[string]trackedFile // map from nickname to metadata

	// Work management.
	//
	// chunkQueue contains a list of incomplete work that the download loop acts
	// upon. The chunkQueue is only ever modified by the main download loop
	// thread, which means it can be accessed and updated without locks.
	//
	// downloadQueue contains a complete history of work that has been
	// submitted to the download loop.
	chunkQueue    []*chunkDownload // Accessed without locks.
	downloadQueue []*download
	newDownloads  chan *download
	newUploads    chan *file
	workerPool    map[types.FileContractID]*worker

	// Memory management - baseMemory tracks how much memory the renter is
	// allowed to consume, memoryAvailable tracks how much more memory the
	// renter can allocate before hitting the cap, and newMemory is a channel
	// used to inform sleeping threads (the download loop and upload loop) that
	// memory has become available.
	baseMemory      uint64
	memoryAvailable uint64
	newMemory       chan struct{}

	// Utilities.
	cs             modules.ConsensusSet
	hostContractor hostContractor
	hostDB         hostDB
	log            *persist.Logger
	persistDir     string
	mu             *siasync.RWMutex
	heapWG         sync.WaitGroup // in-progress chunks join this waitgroup
	tg             threadgroup.ThreadGroup
	tpool          modules.TransactionPool

	lastEstimation modules.RenterPriceEstimation // used to cache the last price estimation result
}

// New returns an initialized renter.
func New(g modules.Gateway, cs modules.ConsensusSet, wallet modules.Wallet, tpool modules.TransactionPool, persistDir string) (*Renter, error) {
	hdb, err := hostdb.New(g, cs, persistDir)
	if err != nil {
		return nil, err
	}
	hc, err := contractor.New(cs, wallet, tpool, hdb, persistDir)
	if err != nil {
		return nil, err
	}

	return newRenter(cs, tpool, hdb, hc, persistDir)
}

// newRenter initializes a renter and returns it.
func newRenter(cs modules.ConsensusSet, tpool modules.TransactionPool, hdb hostDB, hc hostContractor, persistDir string) (*Renter, error) {
	if cs == nil {
		return nil, errNilCS
	}
	if tpool == nil {
		return nil, errNilTpool
	}
	if hc == nil {
		return nil, errNilContractor
	}
	if hdb == nil && build.Release != "testing" {
		return nil, errNilHdb
	}

	r := &Renter{
		files:    make(map[string]*file),
		tracking: make(map[string]trackedFile),

		newDownloads: make(chan *download),
		newUploads:   make(chan *file),
		workerPool:   make(map[types.FileContractID]*worker),

		baseMemory:      defaultMemory,
		memoryAvailable: defaultMemory,
		newMemory:       make(chan struct{}, 1),

		cs:             cs,
		hostDB:         hdb,
		hostContractor: hc,
		persistDir:     persistDir,
		mu:             siasync.New(modules.SafeMutexDelay, 1),
		tpool:          tpool,
	}
	if err := r.initPersist(); err != nil {
		return nil, err
	}

	// Subscribe to the consensus set.
	err := cs.ConsensusSetSubscribe(r, modules.ConsensusChangeRecent, r.tg.StopChan())
	if err != nil {
		return nil, err
	}

	// Spin up the workers for the work pool.
	r.managedUpdateWorkerPool()
	go r.threadedRepairScan()
	go r.threadedDownloadLoop()

	// Kill workers on shutdown.
	r.tg.OnStop(func() error {
		id := r.mu.RLock()
		for _, worker := range r.workerPool {
			close(worker.killChan)
		}
		r.mu.RUnlock(id)
		return nil
	})

	return r, nil
}

// managedMemoryAvailableAdd adds the amount provided to the renter's total
// memory available.
func (r *Renter) managedMemoryAvailableAdd(amt uint64) {
	id := r.mu.Lock()
	r.memoryAvailable += amt
	if r.memoryAvailable > r.baseMemory {
		r.mu.Unlock(id)
		r.log.Critical("Memory available now exceeds base memory:", r.memoryAvailable, r.baseMemory)
		return
	}
	r.mu.Unlock(id)

	// Create a notification that more memory is available.
	select {
	case r.newMemory <- struct{}{}:
	default:
	}
}

// managedMemoryAvailableGet returns the current amount of memory available to
// the renter.
func (r *Renter) managedMemoryAvailableGet() uint64 {
	id := r.mu.RLock()
	memAvail := r.memoryAvailable
	r.mu.RUnlock(id)
	return memAvail
}

// managedMemoryAvailableSub subtracts the amount provided from the renter's
// total memory available.
func (r *Renter) managedMemoryAvailableSub(amt uint64) {
	id := r.mu.Lock()
	if r.memoryAvailable < amt {
		r.mu.Unlock(id)
		r.log.Critical("Memory available is underflowing", r.memoryAvailable, amt)
		return
	}
	r.memoryAvailable -= amt
	r.mu.Unlock(id)
}

// Close closes the Renter and its dependencies
func (r *Renter) Close() error {
	r.tg.Stop()
	r.hostDB.Close()
	return r.hostContractor.Close()
}

// PriceEstimation estimates the cost in siacoins of performing various storage
// and data operations.
//
// TODO: Make this function line up with the actual settings in the renter.
// Perhaps even make it so it uses the renter's actual contracts if it has any.
func (r *Renter) PriceEstimation() modules.RenterPriceEstimation {
	id := r.mu.RLock()
	lastEstimation := r.lastEstimation
	r.mu.RUnlock(id)
	if !reflect.DeepEqual(lastEstimation, modules.RenterPriceEstimation{}) {
		return lastEstimation
	}

	// Grab hosts to perform the estimation.
	hosts := r.hostDB.RandomHosts(priceEstimationScope, nil)

	// Check if there are zero hosts, which means no estimation can be made.
	if len(hosts) == 0 {
		return modules.RenterPriceEstimation{}
	}

	// Add up the costs for each host.
	var totalContractCost types.Currency
	var totalDownloadCost types.Currency
	var totalStorageCost types.Currency
	var totalUploadCost types.Currency
	for _, host := range hosts {
		totalContractCost = totalContractCost.Add(host.ContractPrice)
		totalDownloadCost = totalDownloadCost.Add(host.DownloadBandwidthPrice)
		totalStorageCost = totalStorageCost.Add(host.StoragePrice)
		totalUploadCost = totalUploadCost.Add(host.UploadBandwidthPrice)
	}

	// Convert values to being human-scale.
	totalDownloadCost = totalDownloadCost.Mul(modules.BytesPerTerabyte)
	totalStorageCost = totalStorageCost.Mul(modules.BlockBytesPerMonthTerabyte)
	totalUploadCost = totalUploadCost.Mul(modules.BytesPerTerabyte)

	// Factor in redundancy.
	totalStorageCost = totalStorageCost.Mul64(3) // TODO: follow file settings?
	totalUploadCost = totalUploadCost.Mul64(3)   // TODO: follow file settings?

	// Perform averages.
	totalContractCost = totalContractCost.Div64(uint64(len(hosts)))
	totalDownloadCost = totalDownloadCost.Div64(uint64(len(hosts)))
	totalStorageCost = totalStorageCost.Div64(uint64(len(hosts)))
	totalUploadCost = totalUploadCost.Div64(uint64(len(hosts)))

	// Take the average of the host set to estimate the overall cost of the
	// contract forming.
	totalContractCost = totalContractCost.Mul64(uint64(priceEstimationScope))

	// Add the cost of paying the transaction fees for the first contract.
	_, feePerByte := r.tpool.FeeEstimation()
	totalContractCost = totalContractCost.Add(feePerByte.Mul64(1000).Mul64(uint64(priceEstimationScope)))

	est := modules.RenterPriceEstimation{
		FormContracts:        totalContractCost,
		DownloadTerabyte:     totalDownloadCost,
		StorageTerabyteMonth: totalStorageCost,
		UploadTerabyte:       totalUploadCost,
	}

	id = r.mu.Lock()
	r.lastEstimation = est
	r.mu.Unlock(id)

	return est
}

// SetSettings will update the settings for the renter.
func (r *Renter) SetSettings(s modules.RenterSettings) error {
	err := r.hostContractor.SetAllowance(s.Allowance)
	if err != nil {
		return err
	}

	r.managedUpdateWorkerPool()
	return nil
}

// hostdb passthroughs
func (r *Renter) ActiveHosts() []modules.HostDBEntry                      { return r.hostDB.ActiveHosts() }
func (r *Renter) AllHosts() []modules.HostDBEntry                         { return r.hostDB.AllHosts() }
func (r *Renter) Host(spk types.SiaPublicKey) (modules.HostDBEntry, bool) { return r.hostDB.Host(spk) }
func (r *Renter) ScoreBreakdown(e modules.HostDBEntry) modules.HostScoreBreakdown {
	return r.hostDB.ScoreBreakdown(e)
}
func (r *Renter) EstimateHostScore(e modules.HostDBEntry) modules.HostScoreBreakdown {
	return r.hostDB.EstimateHostScore(e)
}

// contractor passthroughs
func (r *Renter) Contracts() []modules.RenterContract        { return r.hostContractor.Contracts() }
func (r *Renter) CurrentPeriod() types.BlockHeight           { return r.hostContractor.CurrentPeriod() }
func (r *Renter) PeriodSpending() modules.ContractorSpending { return r.hostContractor.PeriodSpending() }
func (r *Renter) Settings() modules.RenterSettings {
	return modules.RenterSettings{
		Allowance: r.hostContractor.Allowance(),
	}
}
func (r *Renter) AllContracts() []modules.RenterContract {
	return r.hostContractor.(interface {
		AllContracts() []modules.RenterContract
	}).AllContracts()
}
func (r *Renter) ProcessConsensusChange(cc modules.ConsensusChange) {
	id := r.mu.Lock()
	r.lastEstimation = modules.RenterPriceEstimation{}
	r.mu.Unlock(id)
}

// Enforce that Renter satisfies the modules.Renter interface.
var _ modules.Renter = (*Renter)(nil)
