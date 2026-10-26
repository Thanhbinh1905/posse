# Workspace Project Workers open as tabs of the Lead's workspace

*Amended by [ADR 0012](0012-group-repository-workers-under-lead.md) and superseded for repository Worker placement by [ADR 0013](0013-all-workers-open-as-lead-tabs.md): every new Worker opens as a tab of its Lead workspace. Existing linked-child Workers remain supported until teardown.*

Each Worker used to open in its own Herdr workspace ([ADR 0007](0007-isolate-workers-from-the-user-session.md) layer 1). With several Workers the sidebar held one row per Worker, interleaved with the User's own workspaces, and every launch or teardown renamed sibling Worker workspaces to keep the `├─`/`└─` connectors in order. The User chose one shared workspace for the Lead and its Workers.

## Decision

- **Launch.** `start` and `relaunch` open the Sandbox with `tab.create` in the Lead's workspace: cwd = the Sandbox, env `POSSE_WORKER_HOME=<home>`, `focus=false`, initial tab label = `<name>-t<n>`. The pane gets its Task label and resolved harness as `display_agent` before `agent.start`, so an interrupted command leaves a pane recovery and Teardown recognize without exposing the internal agent name. After each start and relaunch, posse recomputes pane metadata token `posse_row` for Posse Task panes in Herdr tab order: `├─ <name>-t<n>` for every Worker except the last, which is `└─ <name>-t<n>`. Worker tab labels are `<name>-t<n>` (the suffix is not repeated when the name already ends with it) and the Lead tab is `Lead`. The Lead's `posse_row` is `Lead:<project>`. Posse reports `posse_row` before `agent.start` and without a TTL; relabel failures are logged and do not fail start, relaunch or Teardown. This label reconciliation renames no User tab or workspace. Without an open Lead workspace, `start` refuses with `lead_missing` instead of opening another workspace.
- **Identity from a fresh snapshot.** Herdr renumbers pane and tab ids when it restores a session and reuses workspace ids after a restart, so a recorded id alone never identifies anything. A Task's pane is found by its label; a recorded pane id counts only for an unlabeled pane. The Lead's workspace is the one holding the Lead pane by its label, or a recorded workspace still labeled `Lead:<project>`. Lead recovery does not adopt a reused workspace id.
- **Tab-scoped Teardown.** A Task owns the tabs that hold its labeled pane. Inside those tabs an unlabeled pane whose cwd is in the Sandbox is also the Task's; a pane anywhere else is not, even inside the Sandbox. A tab whose panes are all the Task's closes with `tab.close`. A tab with any other pane keeps it: only the Task's panes close, and the others are reported as `foreign_panes`. posse never calls `workspace.close`. `tab_not_found` counts as already closed. When the User was watching the closed Worker, focus returns to the Lead.
- **Older Tasks.** A Task launched into its own workspace still reconciles by label and tears down by the same rule; closing its last tab removes that workspace.

## Failure domain

The shared workspace changes what an outside close can reach. The User accepted this topology:

- `workspace close` of the Lead's workspace, by the User, the Lead agent or any unguarded path, closes the Lead and every Worker.
- Herdr groups a workspace once a worktree is opened from it. If the Lead's workspace is grouped, `workspace close --group` on any workspace of that repository closes the Lead and every Worker. This is the 2026-09-24 incident class; layer 1 of ADR 0007 no longer confines it to the Lead.
- `tab close` of one Worker tab closes only that Worker. posse's own Teardown can reach neither the Lead nor a sibling.

The Worker guard (ADR 0007 layer 3) is unchanged and still refuses `herdr tab close`, `tab move` and `workspace close` from a Worker's tool calls, including with the Worker's own `$HERDR_TAB_ID` and `$HERDR_WORKSPACE_ID`, which now name the Lead's workspace.

## Considered options

- **One posse-created Worker workspace per Project, kept below the Lead** (the t60 investigation's recommendation). It keeps the Lead out of every Worker close and out of a Lead group close. The User preferred a single workspace. It also needs a workspace marker, since Herdr reuses ids.
- **Keep one workspace per Worker and only move it below the Lead.** It fixes the interleaving but keeps one row per Worker and the sibling relabeling.

## Consequences

- One Agent sidebar row per Lead or Worker. The tab bar shows `Lead` and plain Worker names; the optional Posse Agents layout hides the workspace token when it begins with `Lead:` and renders the bold `$posse_row` pane token as main text (`Lead:<project>` or the tree-prefixed Worker name). Other workspace names remain visible; non-Posse agents show the workspace without a tab name in this row. `posse setup` offers this layout with confirmation on Herdr 0.9.1 or newer (0.9.0 cannot parse token rules), preserves comments, and prints a snippet rather than replacing existing custom Agents rows. `posse setup --check` and `posse doctor` report its status. Labels are recomputed after start, relaunch and Teardown, and reconcile restores them after a Herdr restart. User tabs keep their labels, and Worker connectors never rename a workspace.
- `HERDR_WORKSPACE_ID` in a Worker is the Lead's workspace id. A Worker pane reports its resolved harness as `display_agent` before `agent.start`, so the sidebar never shows the internal agent name.
- Herdr does not persist per-pane env across a restart. A restored Worker is recognized as a Worker by its Sandbox cwd and Task pane (ADR 0007 layer 2), as before.
- Sandbox release still stops every process running in the Sandbox, including a User shell that `cd`'d there from another tab.
