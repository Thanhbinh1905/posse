# Review of PR #35 (t72): Land merged pull requests from any Task state

Reviewed head: `52b52f3` (branch `posse/land-merged-pull`), against issue #12, ADR 0011 and CONTEXT.md.
Scope: `git diff origin/main...52b52f3` (28 files).

## Summary

The change fixes the reported repository case: a `pr` Task in `working`, `needs-decision`, `lost`, `failed` or `stalled` now Lands and is torn down when its PR merges, and the Leftover snapshot covers tracked and untracked non-ignored work for a repository Mount. `go test ./...` passes and the new E2E tests pass.

The review found 15 confirmed defects. Four of them can silently skip a merged PR or discard unlanded work:

1. A workspace member PR that merges while its repo state is `open` is never landed, and polling for it stops (D1).
2. Uncommitted work in non-PR workspace members is discarded without approval when a workspace Task Lands from `working` (D2).
3. A Task whose latest observation is already `MERGED` is never watched again. Databases from the previous release hit this (D3).
4. A closed PR stops all polling for that Task. A reopened PR, or a new PR published after the close, is never watched (D4, D5).

CI lint fails on the branch (D6). The Lookout tab is not restored after a Herdr restart or after its process exits (D7).

Repro tests live in `internal/app/t76_repro_test.go` and `internal/e2e/t76_repro_test.go` on this review branch. At `52b52f3` each one fails, which shows the defect. The exception is `TestT76ConcurrentLookoutTeardownRaisesNoFalseReason`, which passed (see D15).

## Test runs

| Run | Result |
|---|---|
| `go test ./...` (unit) | pass |
| `go test -tags e2e ./internal/e2e` (full, first run) | 2 failures: `TestPRLandingLifecycleAndExternalMerge` (`pr_lifecycle_test.go:110`, t2 still `landed` after `show`) and `TestPosseSpawnNoticeLandTeardownAndRecovery` (`posse_test.go:704`, focused Lead Notice count) |
| Same two tests, `-count=3`, branch and `origin/main` | pass on both |
| Full E2E again, branch | `TestPosseSpawnNoticeLandTeardownAndRecovery` fails again at `posse_test.go:704` |
| Full E2E, `origin/main` | pass |
| `staticcheck -tags=e2e,perf ./...` (CI lint step) | fails, see D6 |
| `go vet -tags=e2e,perf ./...` | pass |

## Confirmed defects

### D1. A workspace member PR merged while in repo state `open` never Lands, and polling for it stops (high)

`pollWorkspacePullRequests` now watches members in any repo state except `landed` and `unchanged` (`workspace_land.go:471`). `RecordMemberPRObservation` still moves a member to `landed` only `AND state='landing'` (`store/workspace.go:249`). A member published with `posse publish --repo` stays `open` until `posse land` gates it. This is the t59 scenario in a workspace: the Rider published, and the User merged before any Land. When that member merges:

- the `MERGED` observation is inserted and `pr_merged` is raised,
- the repo row is left `open`, so the Task does not settle,
- on the next tick `previous.State == "MERGED"` skips the member (`workspace_land.go:484`).

The Task stays unlanded with its Mount held, and no reason is shown. This breaks "Nothing is skipped silently."

Repro: `TestT76WorkspaceOpenMemberMergedLands` gives `state=done worker=open notices=[task_done pr_merged root_behind]` after three ticks.

The existing test `TestWorkspacePullRequestMemberLandsWhenMerged` misses this case: it moves only the Task to `working` and leaves the member in `landing`.

### D2. Workspace Teardown discards uncommitted work in non-PR members without approval (high)

`leftover.snapshot` in `unsaddleTask` (`land.go:423-446`) snapshots only members with `State == landed && PRURL != ""`. `releaseWorkspaceMount` (`workspace_mount.go:238`) then resets and cleans every member worktree without checking it. Before this PR a workspace Task Landed only from `landing`. Now it Lands from `working` (a follow-up after `posse send`), so a Rider can be editing other members when the PR member merges. Those edits can be in a `local` member that already Landed, in an `unchanged` member, or in a member the Task did not request. All of them are lost. This breaks the invariant that unlanded work is never discarded without approval.

Repro: `TestT76WorkspaceTeardownKeepsNonPRMemberWork`. The worker PR merges while the Task is `working` after a follow-up, and an untracked edit exists in the landed local member `e2e-tool`. After Teardown the file is gone and `e2e-tool` has no Leftover ref.

### D3. Upgrade: a Task whose latest observation is `MERGED` is never watched or Landed (high)

`pollProjectPullRequests` drops any Task whose latest observation is `MERGED` (`pull_request.go:590`). The previous release recorded `MERGED` observations while keeping the Task open:

- through the `pr_follow_up_pending` path, for `working`, `needs-decision` and unsafe `done` Tasks,
- through `RestoreMergedTaskWithWork`, which moved a Task from `landed` back to `working`.

After upgrade those Tasks are neither watched nor Landed, and they get no Notice. This is the silent-skip bug the PR sets out to remove, and it remains for exactly the upgraded Tasks. The invariant test `TestMergedPRNeverSilentlySkipsTask` does not seed a prior observation.

Repro: `TestT76UpgradeWorkingTaskWithRecordedMergeLands` gives `state=working notices=[]` after three ticks and Teardowns.

On the User's current `~/.posse/posse.db` (read-only query), no Task is in this state today. The affected Tasks all have `OPEN` or `CLOSED` observations. Other installations that ran 2bcd6e3 can be affected.

### D4. A PR closed and then reopened is never watched again, and `posse land` stays refused (high)

After a `CLOSED` observation, the watch loop only re-raises the Decision and never polls again (`pull_request.go:586-589`; workspace `:708-713`). `landPullRequest` refuses while the latest observation is `CLOSED` (`pull_request.go:98-105`). `posse decide` only records the answer; nothing clears the state. The Decision option `reopen-relaunch` therefore cannot work. If the User reopens and merges the PR, the Task never Lands and gets no Notice.

Repro: `TestT76ReopenedPRIsWatchedAgain` gives `state=working`, and no `gh` call is made after the close.

### D5. A new PR after a closed one is never watched, and a false `pr_closed` Decision names it (high)

`LatestPRObservation` is keyed by `task_id` only. The watch loop compares `previous.State` without checking `previous.PRURL == task.PRURL` (`pull_request.go:581-590`). The same applies to `MERGED` at `:590` and to the workspace path at `:704-716`. The sequence:

1. PR A closes.
2. The Rider publishes PR B and reports `holler done --pr B`, which updates `tasks.pr_url` (`store.go:1005-1008`).
3. The next tick raises "Pull request B closed without merging" and never polls B.
4. If the User merges B, the Task never Lands.

Repro: `TestT76NewPRAfterClosedOneIsWatched` gives `state=working`, with a Decision `pr_closed:https://github.com/acme/shop/pull/17` raised for the open PR.

### D6. CI lint fails on the branch (medium)

CI runs `staticcheck -tags=e2e,perf ./...` (`.github/workflows/ci.yml:55`):

```
internal/app/leftover.go:61:11: error strings should not be capitalized (ST1005)
internal/app/lookout_tab.go:43:10: error strings should not be capitalized (ST1005)
internal/app/teardown_panes.go:74:6: func safePRMergeTeardown is unused (U1000)
```

`gh pr checks 35` reports no checks on the branch, so CI has not flagged this yet.

### D7. The Lookout tab is not restored after a Herdr restart or after its process exits (medium)

`ensureLookoutTab` returns as soon as a pane with the Lookout label exists (`lookout_tab.go:21-25`). It never checks that `posse lookout --poll-only` is still running. It is called only from `posse up` (`lead.go:460`), not from Lead recovery (`restartLead`, `recovery.go:432`). `watchPullRequestsInLookoutTab` exits on the first `prepareProject` error, such as a Herdr protocol error, a missing Project path or a config error (`lookout_tab.go:84-86`). The shell tab then stays open with its label.

- E2E `TestT76LookoutRestoredAfterHerdrRestart`: after an isolated Herdr restart and `posse recover --all`, the Lead is restarted. Herdr restores the labelled Lookout pane but no poll-only process runs (`panes=1 processes=0`). No later `posse up` restarts it either, because the label exists.
- E2E `TestT76LookoutExitIsNeverRestarted`: after the Lookout process gets SIGINT, the labelled tab remains (`Lookout panes after exit: 1`). Nothing restarts polling. The fixture's fake Lead could not be re-`up`ed (`lead_running`), so the `posse up` leg of this repro is based on the code at `lookout_tab.go:21-25`.

Impact: after any Herdr restart or Lookout crash, a codex or prompt Lead that is idle is back to the original issue: nothing polls or tears down.

### D8. Concurrent Teardown leaves a false "teardown incomplete" reason on a torn-down Task (medium)

The PR removed the `intent_active` skip in `autoTeardownLandedTasks` (`teardown_panes.go:65-68`). Automatic Teardown now runs from several processes at once:

- the Lookout tab every 2 s,
- the Lead's `posse lookout` loop (`ops.go:110`),
- every Lead command through `prepareProject`,
- `posse send` (`tasks.go:854`).

When one of them holds the `unsaddle` intent, another records `unsaddle_incomplete: Task already has an unfinished command`. The first then finishes the Teardown, and the false reason stays on the torn-down Task as an unacked Notice for the Lead. A `relaunch` that is in progress when the PR merges produces the same Notice. `recordUnsaddleIncomplete` also deduplicates against acked Notices (`teardown_panes.go:353`), so a real repeat of the same cause after an ack is suppressed.

Repro: `TestT76ConcurrentTeardownRaisesNoFalseReason` holds an `unsaddle` intent from a live parent PID. The result is `state=torn-down` with a reason `teardown incomplete: Task already has an unfinished command`.

### D9. The Lookout types a bare `posse` and inherits neither the binary nor POSSE_HOME (medium)

`ensureLookoutTab` sends the text `posse lookout --poll-only` into a new shell (`lookout_tab.go:58`). Other injected commands use the absolute `os.Executable()` (`lead_delivery.go:170`, `lead_exec.go:40`). The new tab gets the Herdr server's environment, not the Lead's. With a non-default `POSSE_HOME`, a development build, or `posse` missing from the server `PATH`, the Lookout polls the wrong home or exits with `project_not_found`, and no Notice is raised. The E2E passes only because the isolated Herdr server was started with the fixture's `PATH` and `POSSE_HOME`.

### D10. `posse send` to a workspace PR Task does not refresh its PRs (medium)

The refresh runs only when `task.PRURL != ""` (`tasks.go:850`). Workspace Tasks keep member URLs in `task_repos`, and `holler done` for a workspace carries no `--pr`, so `tasks.pr_url` is empty. A `send` to a workspace Task whose member PR merged is delivered, and `pr_merged` is not returned. This misses the acceptance item "`posse send` to a `pr` Task refreshes the PR first". Evidence is from code.

### D11. A closed PR leaves a live `land_ready` Decision (low, regression)

Before this PR, a closed PR moved a `landing` Task to `done`, and `ObsoleteResolvedDecisions` then retired its `land_ready` Decision. Now the Task stays `landing` (`pull_request.go:995-998`), and the obsoleting rule still keys on `t.state<>'landing'` (`store/decisions.go:205`). The User is offered both "Land?" (`land`/`wait`) and the `pr_closed` Decision. Answering `land` leads to a refused `posse land`.

Repro: `TestT76ClosedPRObsoletesLandReadyDecision` shows both Decisions pending.

### D12. The guard on direct `git push` refuses ordinary git commands and can be bypassed (low)

`guard.go:365-370` refuses any `git` argv that contains the word `push` anywhere. Repro `TestT76GuardGitPush`:

- False refusals: `git stash push -m wip` (the stash form the Claude Code system prompt tells agents to use), `git commit -m push`, `git log --grep push`, `git checkout -b push`.
- Bypasses that pass: `git -c alias.p=push p origin HEAD`, `git send-pack origin HEAD:refs/heads/main`, `git "$(echo push)" origin HEAD`.

The non-literal case `c=push; git $c` is refused only because the whole command is opaque. `posse publish` itself is not refused.

This matters because head verification trusts any head that is an ancestor of the Mount branch (`pull_request.go:765-770`), with or without a `publish` record. A Rider that pushes through a bypass and opens its own PR still passes verification. The guard is the only enforcement of "push through `posse publish`".

### D13. The Leftover snapshot runs before processes in the Mount are stopped (low)

Order in `unsaddleTask`: `panes.close` → `leftover.snapshot` → `mount.release` (`land.go:414-451`). `releaseMount` calls `stopMountProcesses` first and resets only after it (`remuda.go:446-453`). A background process the Rider started in the Mount (a detached build, test run or watcher) can write non-ignored files after the snapshot and before it is stopped. Those writes are discarded. The snapshot should run after the Mount's processes are stopped. Evidence is from code.

### D14. Leftover and `pr_closed` Decisions have no defined action (low)

`posse decide` records the answer and notifies the Lead to "carry out the chosen action" (`decisions.go:162-203`). Nothing defines that action:

- The Lead instructions (`lead.go:100-116`) do not mention `leftover`.
- `pr_closed` has only "ask the User whether to reopen or discard".
- `posse ride` cannot start from `posse/<name>-leftover`, because it takes only `--brief`, `--name` and `--profile` (`tasks.go:72`).
- No command deletes a Leftover branch for `discard`.
- For `reopen-relaunch`, see D4.

Leftover and `pr_closed` Decisions are never obsoleted, for example when a closed PR's Task is later discarded.

### D15. Existing E2E tests fail on the branch in full-suite runs (medium; the cause is a hypothesis)

`TestPosseSpawnNoticeLandTeardownAndRecovery` failed in both full E2E runs on the branch (2/2). The full suite passed on `origin/main` (1/1). `TestPRLandingLifecycleAndExternalMerge` also failed once. Both tests pass 3/3 when run alone, on the branch and on `origin/main`. Both failures fit the Lookout tab's background `prepareProject` running alongside the test's own commands:

- `pr_lifecycle_test.go:110` expects Teardown right after `show`. With D8, a concurrent Lookout Teardown makes `show` skip it.
- `posse_test.go:704` counts undelivered Notices while the Lookout also delivers them.

This cause is a hypothesis. The E2E repro `TestT76ConcurrentLookoutTeardownRaisesNoFalseReason` runs `show` in a loop alongside the Lookout, and it did not hit the collision in one run. The confirmed part is that these failures happen on the branch and not on `origin/main`.

## Other findings (not defects in the change itself)

- Real-data acceptance ("first reconcile Lands and tears down t13, t59, t61 and t62, and raises Decisions for t46, t48 and t65"), checked read-only against the User's DB, `gh` and the Mounts:
  - t59, t61 and t62 are `lost`, with PRs #5, #6 and #7 `MERGED`, the merged head on the Mount's Task branch, and a clean Mount. They should Land and tear down.
  - t46, t48 and t65 have a `CLOSED` latest observation and will get `pr_closed` Decisions.
  - t13 will not Land. Its URL `…/pull/20` resolves to an issue, not a PR ("Could not resolve to a PullRequest"). Its Mount `mount-5` points to a pruned gitdir (`/data/Personal/posse/.git/worktrees/mount-5`). It will only raise a per-Task `pr_watch_failing`.
- The Lookout tab and `posse lookout --poll-only` are not documented in `docs/spec.md` or CONTEXT.md, and the `lookout` usage string omits `--poll-only` (`service.go:78`). Acceptance asks for spec sections 6, 14 and 15 to be updated.
- A Leftover snapshot errors with a visible reason, retried forever, in two cases: the Mount is off its Task branch (detached, or switched branch), or the merged head is no longer an ancestor (the Rider rebased). No posse command can finish that Teardown, because `teardown --discard` is refused for `landed`. The User must repair the Mount by hand.
- With `--replace` from another workspace, a second Lookout is created in the new workspace while the old one keeps polling. `watchPullRequestsInLookoutTab` checks its workspace only at start. This makes D8 more likely.

## Checked and found sound

- No new path moves a Task to `done`. The `CLOSED → done` effect is gone, and `RecordPRObservation` only moves a Task to `landed`.
- Repository Mounts: the snapshot runs after pane close and before `releaseMount`, and it covers commits past the merged head plus tracked and untracked non-ignored edits. `failSpawn` (`tasks.go:573`) releases directly, but only for `spawning` Tasks without a PR. The interrupted `unsaddle` intent resumes through `unsaddleTask`.
- Signal races: a Signal after the poll Lands is refused by the state check. A poll whose Task changed in flight is rejected by `RecordPRObservation` and retried.
- The Lookout creates its own tab and types only into that tab's root pane. It closes only the tab it just created, and only on failure. It never closes or types into a User tab.
