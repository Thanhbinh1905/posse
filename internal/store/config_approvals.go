package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RecordConfigApproval keeps the User's words with the Project and change they authorized.
// For a global change, projectID identifies the Lead that made it.
func (db *DB) RecordConfigApproval(ctx context.Context, projectID int64, key, action, value, quote string) error {
	if strings.TrimSpace(quote) == "" {
		return fmt.Errorf("config approval needs a User quote")
	}
	var storedValue any
	switch action {
	case "set", "move":
		storedValue = value
	case "unset":
		storedValue = nil
	default:
		return fmt.Errorf("unknown config approval action %q", action)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO config_approvals(project_id,key,action,value,user_quote,at) VALUES(?,?,?,?,?,?)`, projectID, key, action, storedValue, quote, time.Now().UnixMilli())
	return err
}
