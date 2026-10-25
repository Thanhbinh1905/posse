package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func gitlabStewardFixture(t *testing.T) (stewardTarget, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$GLAB_FIXTURE/glab.log"
case "$*" in
  *"projects/acme%2Fshop/jobs/55") cat "$GLAB_FIXTURE/job.json" ;;
  *"projects/acme%2Fshop/pipelines/9/jobs?include_retried=true&per_page=100") cat "$GLAB_FIXTURE/jobs.json" ;;
  *"projects/acme%2Fshop/merge_requests/7/notes?per_page=100") cat "$GLAB_FIXTURE/notes.json" ;;
  *) printf 'unexpected glab invocation: %s\n' "$*" >&2; exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GLAB_FIXTURE", root)
	target := stewardTarget{
		forge: repositoryForge{Kind: "gitlab", Host: "git.example.com", Path: "acme/shop", Root: root},
		prURL: "https://git.example.com/acme/shop/-/merge_requests/7",
	}
	return target, root
}

func writeGlabResponse(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGitLabStewardRerunIsBoundToTheHeadAndObservedByNewerJob(t *testing.T) {
	ctx := context.Background()
	target, root := gitlabStewardFixture(t)
	head := strings.Repeat("a", 40)

	writeGlabResponse(t, root, "job.json", `{"id":55,"name":"unit","status":"failed","pipeline":{"id":9,"sha":"`+head+`"}}`)
	baseline, err := forgeRerunBaseline(ctx, target, "55", head)
	if err != nil || baseline != "9" {
		t.Fatalf("baseline = %q, %v", baseline, err)
	}
	var refusal *axi.Error
	if _, err := forgeRerunBaseline(ctx, target, "55", strings.Repeat("b", 40)); !errors.As(err, &refusal) || refusal.Code != "pr_rerun_refused" {
		t.Fatalf("a job from another head was accepted: %v", err)
	}
	writeGlabResponse(t, root, "job.json", `{"id":55,"name":"unit","status":"running","pipeline":{"id":9,"sha":"`+head+`"}}`)
	if _, err := forgeRerunBaseline(ctx, target, "55", head); !errors.As(err, &refusal) || refusal.Code != "pr_rerun_refused" {
		t.Fatalf("a running job was accepted for rerun: %v", err)
	}

	writeGlabResponse(t, root, "jobs.json", `[{"id":55,"name":"unit","status":"failed"},{"id":56,"name":"lint","status":"success"}]`)
	if applied, err := forgeRerunApplied(ctx, target, "55", baseline); err != nil || applied {
		t.Fatalf("rerun without a newer job applied=%v err=%v", applied, err)
	}
	writeGlabResponse(t, root, "jobs.json", `[{"id":55,"name":"unit","status":"failed"},{"id":61,"name":"unit","status":"pending"}]`)
	if applied, err := forgeRerunApplied(ctx, target, "55", baseline); err != nil || !applied {
		t.Fatalf("newer retried job was not observed: applied=%v err=%v", applied, err)
	}
	writeGlabResponse(t, root, "jobs.json", `not json`)
	if _, err := forgeRerunApplied(ctx, target, "55", baseline); err == nil {
		t.Fatal("an unreadable pipeline was treated as an observed outcome")
	}
}

func TestStewardReplyMarkerIsFoundAmongGitLabNotes(t *testing.T) {
	ctx := context.Background()
	target, root := gitlabStewardFixture(t)
	writeGlabResponse(t, root, "notes.json", `[{"body":"Thanks"},{"body":"Rerun requested.\n\n`+stewardOperationMarker(3)+`"}]`)
	if found, err := forgeCommentHasMarker(ctx, target, stewardOperationMarker(3)); err != nil || !found {
		t.Fatalf("posted reply was not found: found=%v err=%v", found, err)
	}
	// Operation 3's marker must not satisfy operation 31 or 4.
	for _, id := range []int64{4, 31} {
		if found, err := forgeCommentHasMarker(ctx, target, stewardOperationMarker(id)); err != nil || found {
			t.Fatalf("operation %d matched another reply: found=%v err=%v", id, found, err)
		}
	}
}
