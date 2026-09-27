package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDoctorWarnsWhenAnotherRepositoryPrimaryCanCloseTheLeadGroup(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	lead := herdr.Workspace{WorkspaceID: "w1", Label: "Lead:shop"}
	lead.Worktree.RepoKey = "same-repository"
	other := herdr.Workspace{WorkspaceID: "w2", Label: "notes"}
	other.Worktree.RepoKey = lead.Worktree.RepoKey
	child := herdr.Workspace{WorkspaceID: "w3", Label: "rider"}
	child.Worktree.RepoKey = lead.Worktree.RepoKey
	child.Worktree.IsLinkedWorktree = true
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{lead, other, child}, Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead"}}}
	service := testService(home, fake)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"doctor"}); code != 0 {
		t.Fatalf("doctor failed: %d %s", code, &output)
	}
	if !strings.Contains(output.String(), "Herdr group shop") || !strings.Contains(output.String(), "w2") || strings.Contains(output.String(), "another primary workspace w3") {
		t.Fatalf("doctor missed the other primary or warned on a Rider child: %s", &output)
	}
}
