//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedProductionHistory gives the fixture store the history of a Project that
// has run for weeks: hundreds of torn-down Tasks with their Signals,
// transitions and PR observations, thousands of Herdr events, and a backlog of
// unacknowledged Watch failure Notices. Its shape mirrors a real store that
// hit SQLITE_BUSY at max_workers (#428).
func seedProductionHistory(t *testing.T, f *prLifecycleFixture) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed production history: %v\n%s", err, query)
		}
	}
	start := time.Now().Add(-21 * 24 * time.Hour).UnixMilli()
	const historicalTasks = 400
	for i := range historicalTasks {
		seq := 1000 + i
		at := start + int64(i)*60_000
		result, err := tx.ExecContext(ctx, `INSERT INTO tasks(project_id,seq,type,title,state,landing_mode,branch,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			f.project.ID, seq, "change", fmt.Sprintf("Historical Task %d", seq), "torn-down", "pr", fmt.Sprintf("posse/t%d", seq), at, at)
		if err != nil {
			t.Fatalf("seed historical Task: %v", err)
		}
		taskID, _ := result.LastInsertId()
		for step, state := range []string{"spawning", "working", "done", "merging", "torn-down"} {
			exec(`INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(?,?,?,?,?,?)`, taskID, "queued", state, "cli", "historical", at+int64(step))
		}
		for signal := range 6 {
			exec(`INSERT INTO signals(task_id,verb,note,at) VALUES(?,?,?,?)`, taskID, "working", fmt.Sprintf("historical progress %d", signal), at+int64(signal))
		}
		for observation := range 10 {
			exec(`INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,checks,observed_at) VALUES(?,?,?,?,?,?,?)`,
				f.project.ID, taskID, fmt.Sprintf("https://github.com/acme/shop/pull/%d", seq), fmt.Sprintf("%040d", observation), "OPEN", "SUCCESS", at+int64(observation))
		}
		exec(`INSERT INTO messages(task_id,body,created_at,status,delivered_at) VALUES(?,?,?,?,?)`, taskID, "historical instruction", at, "delivered", at+1)
		exec(`INSERT INTO decisions(project_id,task_id,origin,question,options_json,answer,created_at,answered_at) VALUES(?,?,?,?,?,?,?,?)`,
			f.project.ID, taskID, fmt.Sprintf("history:%d", seq), "Historical question?", `["yes","no"]`, "yes", at, at+1)
		for notice := range 8 {
			exec(`INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at,delivered_at,acked_at) VALUES(?,?,?,?,?,?,?,?)`,
				f.project.ID, taskID, "task_done", "Historical Task finished", fmt.Sprintf(`{"pr_url":"https://github.com/acme/shop/pull/%d"}`, seq), at+int64(notice), at+int64(notice), at+int64(notice))
		}
	}
	// Two alternating Watch failures defeat the consecutive-summary dedupe and
	// leave thousands of unacknowledged Project Notices behind.
	for i := range 3700 {
		phase := "maintenance"
		if i%2 == 0 {
			phase = "reconcile"
		}
		exec(`INSERT INTO notices(project_id,kind,summary,data_json,created_at) VALUES(?,?,?,?,?)`,
			f.project.ID, "pr_watch_failing", "Watch "+phase+" failed: config.toml: defaults.review: legacy review policy", `{}`, start+int64(i))
	}
	// A long-running Project has already evaluated its history for Decisions.
	exec(`INSERT INTO decision_notice_cursors(project_id,last_notice_id) SELECT ?,MAX(id) FROM notices WHERE project_id=?
		ON CONFLICT(project_id) DO UPDATE SET last_notice_id=excluded.last_notice_id`, f.project.ID, f.project.ID)
	for i := range 8000 {
		exec(`INSERT INTO events(received_at,kind,pane_id,data_json) VALUES(?,?,?,?)`, start+int64(i), "pane_agent_status_changed", fmt.Sprintf("w1-%d", i%40), `{"agent":"claude","agent_status":"working","source":"posse.fake","workspace_id":"w1"}`)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
