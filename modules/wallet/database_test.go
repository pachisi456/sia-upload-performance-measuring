package wallet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pachisi456/Sia/build"
	"github.com/pachisi456/Sia/modules"

	"github.com/NebulousLabs/bolt"
)

// TestDBOpen tests the wallet.openDB method.
func TestDBOpen(t *testing.T) {
	w := new(Wallet)
	err := w.openDB("")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	testdir := build.TempDir(modules.WalletDir, "TestDBOpen")
	os.MkdirAll(testdir, 0700)
	err = w.openDB(filepath.Join(testdir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	w.db.View(func(tx *bolt.Tx) error {
		for _, b := range dbBuckets {
			if tx.Bucket(b) == nil {
				t.Error("bucket", string(b), "does not exist")
			}
		}
		return nil
	})
	w.db.Close()
}
