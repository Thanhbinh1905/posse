package store

import (
	"context"
	"errors"
	"os"
	"sync"
)

// newStoreTemplate holds one freshly migrated store image for this process.
// Suites that create hundreds of stores opt in, so each new store costs one
// file write instead of replaying every migration with its fsyncs.
var newStoreTemplate struct {
	sync.Mutex
	enabled bool
	image   []byte
}

// SeedNewStoresFromTemplate makes this process create each new store file from
// the first store it migrated. The image is captured right after migration, so
// it holds only migration data. Test binaries call it from TestMain.
func SeedNewStoresFromTemplate() {
	newStoreTemplate.Lock()
	defer newStoreTemplate.Unlock()
	newStoreTemplate.enabled = true
}

// seedNewStore writes the template image to path when path does not exist yet.
// It reports whether path is a new store that should supply the template.
func seedNewStore(path string) (captureTemplate bool) {
	newStoreTemplate.Lock()
	enabled, image := newStoreTemplate.enabled, newStoreTemplate.image
	newStoreTemplate.Unlock()
	if !enabled {
		return false
	}
	// A leftover WAL would be replayed onto the new file; migrate normally.
	for _, existing := range []string{path, path + "-wal"} {
		if _, err := os.Lstat(existing); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	if image == nil {
		return true
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	_, writeErr := file.Write(image)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)
	}
	return false
}

// captureNewStoreTemplate keeps a copy of a store that was just created and
// migrated, unless another store already supplied the template.
func (db *DB) captureNewStoreTemplate(ctx context.Context) {
	newStoreTemplate.Lock()
	defer newStoreTemplate.Unlock()
	if newStoreTemplate.image != nil {
		return
	}
	copyPath := db.Path + ".template"
	_ = os.Remove(copyPath)
	defer os.Remove(copyPath)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, copyPath); err != nil {
		return
	}
	if image, err := os.ReadFile(copyPath); err == nil {
		newStoreTemplate.image = image
	}
}
