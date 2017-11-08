package explorer

import (
	"os"
	"path/filepath"

	"github.com/pachisi456/Sia/encoding"
	"github.com/pachisi456/Sia/modules"
	"github.com/pachisi456/Sia/persist"
	"github.com/pachisi456/Sia/types"

	"github.com/NebulousLabs/bolt"
)

var explorerMetadata = persist.Metadata{
	Header:  "Sia Explorer",
	Version: "0.5.2",
}

// initPersist initializes the persistent structures of the explorer module.
func (e *Explorer) initPersist() error {
	// Make the persist directory
	err := os.MkdirAll(e.persistDir, 0700)
	if err != nil {
		return err
	}

	// Open the database
	db, err := persist.OpenDatabase(explorerMetadata, filepath.Join(e.persistDir, "explorer.db"))
	if err != nil {
		return err
	}
	e.db = db

	// Initialize the database
	err = e.db.Update(func(tx *bolt.Tx) error {
		buckets := [][]byte{
			bucketBlockFacts,
			bucketBlockIDs,
			bucketBlocksDifficulty,
			bucketBlockTargets,
			bucketFileContractHistories,
			bucketFileContractIDs,
			bucketInternal,
			bucketSiacoinOutputIDs,
			bucketSiacoinOutputs,
			bucketSiafundOutputIDs,
			bucketSiafundOutputs,
			bucketTransactionIDs,
			bucketUnlockHashes,
		}
		for _, b := range buckets {
			_, err := tx.CreateBucketIfNotExists(b)
			if err != nil {
				return err
			}
		}

		// set default values for the bucketInternal
		internalDefaults := []struct {
			key, val []byte
		}{
			{internalBlockHeight, encoding.Marshal(types.BlockHeight(0))},
			{internalRecentChange, encoding.Marshal(modules.ConsensusChangeID{})},
		}
		b := tx.Bucket(bucketInternal)
		for _, d := range internalDefaults {
			if b.Get(d.key) != nil {
				continue
			}
			err := b.Put(d.key, d.val)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}
