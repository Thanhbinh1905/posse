package store

import (
	"bytes"
	"os"
	"testing"

	"github.com/BurntSushi/toml"
)

func Test014TaskSnapshotDecodesPersistedAuthorityAndCustody(t *testing.T) {
	data, err := os.ReadFile("testdata/task-v014.toml")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot TaskSnapshot
	metadata, err := toml.Decode(string(data), &snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if keys := metadata.Undecoded(); len(keys) != 0 {
		t.Fatalf("0.14 persisted keys were silently ignored: %v", keys)
	}
	if snapshot.Task.MergeMode != "pr" || snapshot.Task.AutonomyMerge != "ask" || snapshot.Task.SandboxID != 17 || snapshot.Task.MergedRef != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("0.14 snapshot lost Merge authority or Sandbox custody: %#v", snapshot.Task)
	}
}

func TestTaskSnapshotWrites014CompatibleAuthorityAndCustody(t *testing.T) {
	var output bytes.Buffer
	original := Task{MergeMode: "pr", AutonomyMerge: "ask", SandboxID: 17, MergedRef: "0123456789abcdef0123456789abcdef01234567"}
	if err := toml.NewEncoder(&output).Encode(TaskSnapshot{Version: 1, Task: original}); err != nil {
		t.Fatal(err)
	}
	// Decode the emitted data through an independent 0.14 wire model, not
	// Task again: a round trip through the same broken tags would still pass.
	var legacy struct {
		Task struct {
			MergeMode     string `toml:"landing_mode"`
			AutonomyMerge string `toml:"autonomy_land"`
			SandboxID     int64  `toml:"mount_id"`
			MergedRef     string `toml:"landed_ref"`
		} `toml:"task"`
	}
	if _, err := toml.Decode(output.String(), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Task.MergeMode != original.MergeMode || legacy.Task.AutonomyMerge != original.AutonomyMerge || legacy.Task.SandboxID != original.SandboxID || legacy.Task.MergedRef != original.MergedRef {
		t.Fatalf("new snapshot no longer satisfies the persisted 0.14 contract: %#v", legacy.Task)
	}
}
