package contractor

import (
	"time"

	"github.com/pachisi456/Sia/build"
	"github.com/pachisi456/Sia/types"
)

// uptimeMinScans is the minimum number of scans required to judge whether a
// host is offline or not.
const uptimeMinScans = 3

// uptimeWindow specifies the duration in which host uptime is checked.
var uptimeWindow = func() time.Duration {
	switch build.Release {
	case "dev":
		return 30 * time.Minute
	case "standard":
		return 7 * 24 * time.Hour // 1 week.
	case "testing":
		return 15 * time.Second
	}
	panic("undefined uptimeWindow")
}()

// IsOffline indicates whether a contract's host should be considered offline,
// based on its scan metrics.
func (c *Contractor) IsOffline(id types.FileContractID) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isOffline(id)
}

// isOffline indicates whether a contract's host should be considered offline,
// based on its scan metrics.
func (c *Contractor) isOffline(id types.FileContractID) bool {
	// See if there is a contract to match the id in the current set of
	// contracts.
	id = c.resolveID(id)
	contract, ok := c.contracts[id]
	if !ok {
		// No contract, assume offline.
		return true
	}
	// See if there is a host that corresponds to this contract.
	host, ok := c.hdb.Host(contract.HostPublicKey)
	if !ok {
		// No host, assume offline.
		return true
	}
	// See if the host has a scan history.
	if len(host.ScanHistory) < 1 {
		// No scan history, assume offline.
		return true
	}
	// Return 'true' if the most recent scan of the host failed, false
	// otherwise.
	return !host.ScanHistory[len(host.ScanHistory)-1].Success
}
