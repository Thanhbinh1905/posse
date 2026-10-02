package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// MigrationGate is store.MigrationGate for the posse binary. A Worker never
// migrates the posse home it belongs to, even when forced. Otherwise only the
// installed posse (the binary recorded by `posse setup`) migrates a home
// that has one, unless the User forces it.
func (s *Service) MigrationGate(path string, pending []int64, forced bool) error {
	home := filepath.Dir(path)
	if s.workerCaller(context.Background(), home) {
		return &store.SchemaError{Code: "migration_refused", Message: fmt.Sprintf("a Rider cannot migrate the posse home %s (pending migrations %v)", home, pending),
			Help: "Retry after the Lead or the User runs any posse command, which migrates the database"}
	}
	if forced {
		return nil
	}
	db, err := store.OpenReadOnly(home)
	if err != nil {
		return fmt.Errorf("inspect pending migration safety: %w", err)
	}
	idle, idleErr := db.PendingIdleMigrations(context.Background())
	if idleErr == nil {
		waiting := []int64{}
		for _, version := range pending {
			for _, required := range idle {
				if required == version {
					waiting = append(waiting, version)
				}
			}
		}
		idleErr = db.RefuseLiveMigration(context.Background(), waiting, false)
	}
	_ = db.Close()
	if idleErr != nil {
		return idleErr
	}
	manifest, found, err := readSetupManifest(filepath.Join(home, setupManifestName))
	if err != nil || !found || manifest.Binary == "" {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot tell which posse build is running: %w", err)
	}
	if !samePath(self, manifest.Binary) {
		return fmt.Errorf("only the installed posse (%s) migrates %s; this is %s (pending migrations %v)", manifest.Binary, home, self, pending)
	}
	return nil
}

// schemaFailure turns store migration and contention errors into structured CLI errors.
func schemaFailure(err error) error {
	if store.IsBusy(err) {
		return axi.Failure("store_busy", "the store was busy while opening or migrating the database", true, "Retry the command")
	}
	var schema *store.SchemaError
	if errors.As(err, &schema) {
		return axi.Failure(schema.Code, schema.Message, false, schema.Help)
	}
	return err
}
