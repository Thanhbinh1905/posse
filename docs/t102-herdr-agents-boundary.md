# t102: Agents list ownership boundary

## Reproduction

Screenshot: `/tmp/codex-clipboard-8hxYQa.png` shows the workspace Rider adjacent under `Lead:posse`, but the Agents list places `investigate-multi` after three unrelated `clouddbv1` agents without a connector.

Ran an isolated Herdr 0.9.0 server and Posse home using the `newRiderTabsFixture` E2E fixture. The fixture's Lead and Rider were launched through `posse up` and `posse ride`; two unrelated fake agents were started in separate workspaces. Captured an actual attached desktop client through `script` (45 rows, 160 columns). The sidebar displayed:

```text
spaces
  notes
  Lead:shop
    main
    └─ ● investigate-mu…
  scratch
agents              grouped
  ○ notes
  ○ Lead:shop · Lead
  ○ scratch
  ● investigate-multi
```

The live Herdr snapshot had workspace order `[notes(w1), Lead:shop(w2), scratch(w3), investigate-multi(w4)]` and agent order `[foreign-before(w1), posse-shop-lead-1(w2), foreign-after(w3), posse-shop-t1-1(w4)]`; the Rider workspace had `is_linked_worktree=true` and the same `repo_key` as the Lead. The corresponding standalone existing test `go test -tags=e2e ./internal/e2e -run '^TestRidersAsGroupedChildrenRecoverAfterAnotherPrimaryClosesGroup$' -count=1 -v` passed (34.16 s), but its UI assertion checks only presence of names, not their adjacency/connector. The isolated capture reproduced both user-visible defects. Temporary diagnostic test and capture were removed.

## Boundary

- `internal/app/rider_tab.go` uses `worktree.open` with the Lead's `workspace_id`, the Mount `path`, and `focus=false`; it reports Rider pane metadata and a `posse_row` token. `internal/app/tab_labels.go` intentionally skips linked worktree children when assigning synthetic tree tokens for legacy tabs. `internal/app/lead.go` reports `Lead:<project>` metadata. Posse already gives Herdr native worktree parentage, and Herdr's Spaces section renders it correctly.
- Herdr 0.9.0's default `[ui.sidebar.agents]` rows are `[["state_icon", "machine", "workspace", "tab"], ["agent"]]`. These rows do not include Posse's `$posse_row` token. Posse's optional custom Agents layout is retired in `internal/app/setup.go`, and `internal/app/sidebar_layout.go` leaves user-defined layouts alone. Adding a token alone cannot change the default rendered row.
- Herdr's bundled socket schema exposes `agent.view.set` sorting by `workspace_order`, `tab_order`, `pane_order`, status/attention, or a token. None is a native parent-child/group sort, and a Posse-wide view would reorder/filter unrelated agents and override user navigation/view choices. Pane metadata accepts generic `tokens`, not a supported parent-agent relationship. Posse cannot safely move unrelated agents, rewrite user configuration, or reorder workspace creation merely to simulate Agents hierarchy.

**Required owner/scope:** Herdr Agents sidebar presentation. It should derive agent order and tree connectors from the same native linked-worktree topology already shown in Spaces (including separate projects, multiple Leads/Riders and unrelated agents), while preserving selection/focus and custom view behavior. Posse changes are not justified unless Herdr adds a supported parent/child integration that Posse must populate.

## Posse diagnostic issue

The Rider command guard refused a direct isolated Herdr experiment even with `env -i HOME=/tmp/posse-t102-lab/home XDG_CONFIG_HOME=/tmp/posse-t102-lab/xdg POSSE_HOME=/tmp/posse-t102-lab/posse PATH=/usr/local/bin:/usr/bin:/bin /home/thanhbinh/.nix-profile/bin/herdr --session t102-lab agent`, stating it "cannot tell which Herdr server it reaches." Expected: allow a clearly isolated server; actual: refusal before invocation. Workaround: run the project's existing E2E fixture, which starts a separate isolated Herdr server and never touches the User's panes or settings. This guard issue is independent of the Agents rendering defect.
