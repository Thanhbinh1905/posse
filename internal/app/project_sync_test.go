package app

import (
	"reflect"
	"testing"
)

func TestGitPathCollisions(t *testing.T) {
	tests := []struct {
		name     string
		upstream []string
		local    []string
		want     []string
	}{
		{
			name:     "unrelated untracked path is safe",
			upstream: []string{"src/main.go"},
			local:    []string{"AGENTS.md"},
		},
		{
			name:     "same path conflicts",
			upstream: []string{"AGENTS.md"},
			local:    []string{"AGENTS.md"},
			want:     []string{"AGENTS.md"},
		},
		{
			name:     "file and directory paths conflict",
			upstream: []string{"cache/index"},
			local:    []string{"cache"},
			want:     []string{"cache"},
		},
		{
			name:     "incoming file conflicts with local directory content",
			upstream: []string{"cache"},
			local:    []string{"cache/index"},
			want:     []string{"cache/index"},
		},
		{
			name:     "duplicate incoming paths appear once",
			upstream: []string{"AGENTS.md", "AGENTS.md"},
			local:    []string{"AGENTS.md"},
			want:     []string{"AGENTS.md"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := gitPathCollisions(test.upstream, test.local); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("gitPathCollisions(%q, %q) = %q, want %q", test.upstream, test.local, got, test.want)
			}
		})
	}
}

func TestRootSyncBlockReason(t *testing.T) {
	tests := []struct {
		name                string
		tracked, collisions []string
		want                string
	}{
		{
			name: "clean tracked state with unrelated untracked paths is safe",
			want: "",
		},
		{
			name:    "tracked changes block regardless of incoming paths",
			tracked: []string{"README.md"},
			want:    `tracked local changes: "README.md"`,
		},
		{
			name:       "untracked collision blocks",
			collisions: []string{"AGENTS.md"},
			want:       `local ignored or untracked paths conflict with incoming changes: "AGENTS.md"`,
		},
		{
			name:       "tracked changes take priority over untracked collisions",
			tracked:    []string{"README.md"},
			collisions: []string{"AGENTS.md"},
			want:       `tracked local changes: "README.md"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := rootSyncBlockReason(test.tracked, test.collisions)
			if got != test.want {
				t.Fatalf("rootSyncBlockReason() = %q, want %q", got, test.want)
			}
		})
	}
}
