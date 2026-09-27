# Workspace Project Riders open as tabs of the Lead's workspace

*Amended by [ADR 0012](0012-group-repository-riders-under-lead.md): repository Riders now open as linked worktree children of the Lead, while Workspace Project Riders and legacy Tasks keep this tab behavior.*

Each Rider used to open in its own Herdr workspace ([ADR 0007](0007-isolate-workers-from-the-user-session.md) layer 1). With several Riders the sidebar held one row per Rider, interleaved with the User's own workspaces, and every launch or teardown renamed sibling Rider workspaces to keep the `├─`/`└─` connectors in order. The User chose one shared workspace for the Lead and its Riders.

## Decision

- **Launch.** `ride` and `relaunch` open the Mount with `tab.create` in the Lead's workspace: cwd = the Mount, env `POSSE_WORKER_HOME=<home>`, `focus=false`, initial tab label = `<name>`. The pane gets its Task label and resolved harness as `display_agent` before `agent.start`, so an interrupted command leaves a pane recovery and Teardown recognize without exposing the internal agent name. After each ride and relaunch, posse recomputes pane metadata token `posse_row` for Posse Task panes in Herdr tab order: `├─ <name>` for every Rider except the last, which is `└─ <name>`. Tab labels stay plain (`<name>` for Riders and `Lead` for the Lead). The Lead's `posse_row` is `Lead:<project>`. Posse reports `posse_row` before `agent.start` and without a TTL; relabel failures are logged and do not fail ride, relaunch or Teardown. This label reconciliation renames no User tab or workspace. Without an open Lead workspace, `ride` refuses with `lead_missing` instead of opening another workspace.
- **Identity from a fresh snapshot.** Herdr renumbers pane and tab ids when it restores a session and reuses workspace ids after a restart, so a recorded id alone never identifies anything. A Task's pane is found by its label; a recorded pane id counts only for an unlabeled pane. The Lead's workspace is the one holding the Lead pane by its label, or a recorded workspace still labeled `Lead:<project>`. Lead recovery does not adopt a reused workspace id.
- **Tab-scoped Teardown.** A Task owns the tabs that hold its labeled pane. Inside those tabs an unlabeled pane whose cwd is in the Mount is also the Task's; a pane anywhere else is not, even inside the Mount. A tab whose panes are all the Task's closes with `tab.close`. A tab with any other pane keeps it: only the Task's panes close, and the others are reported as `foreign_panes`. posse never calls `workspace.close`. `tab_not_found` counts as already closed. When the User was watching the closed Rider, focus returns to the Lead.
- **Older Tasks.** A Task launched into its own workspace still reconciles by label and tears down by the same rule; closing its last tab removes that workspace.

## Failure domain

The shared workspace changes what an outside close can reach. The User accepted this topology:

- `workspace close` of the Lead's workspace, by the User, the Lead agent or any unguarded path, closes the Lead and every Rider.
- Herdr groups a workspace once a worktree is opened from it. If the Lead's workspace is grouped, `workspace close --group` on any workspace of that repository closes the Lead and every Rider. This is the 2026-09-24 incident class; layer 1 of ADR 0007 no longer confines it to the Lead.
- `tab close` of one Rider tab closes only that Rider. posse's own Teardown can reach neither the Lead nor a sibling.

The Worker guard (ADR 0007 layer 3) is unchanged and still refuses `herdr tab close`, `tab move` and `workspace close` from a Rider's tool calls, including with the Rider's own `$HERDR_TAB_ID` and `$HERDR_WORKSPACE_ID`, which now name the Lead's workspace.

## Considered options

- **One posse-created Rider workspace per Project, kept below the Lead** (the t60 investigation's recommendation). It keeps the Lead out of every Rider close and out of a Lead group close. The User preferred a single workspace. It also needs a workspace marker, since Herdr reuses ids.
- **Keep one workspace per Rider and only move it below the Lead.** It fixes the interleaving but keeps one row per Rider and the sibling relabeling.

## Consequences

- One Agent sidebar row per Lead or Rider. The tab bar shows `Lead` and plain Rider names; the optional Posse Agents layout hides the workspace token when it begins with `Lead:` and renders the bold `$posse_row` pane token as main text (`Lead:<project>` or the tree-prefixed Rider name). Other workspace names remain visible; non-Posse agents show the workspace without a tab name in this row. `posse setup` offers this layout with confirmation on Herdr 0.9.1 or newer (0.9.0 cannot parse token rules), preserves comments, and prints a snippet rather than replacing existing custom Agents rows. `posse setup --check` and `posse doctor` report its status. Labels are recomputed after ride, relaunch and Teardown, and reconcile restores them after a Herdr restart. User tabs keep their labels, and Rider connectors never rename a workspace.
- `HERDR_WORKSPACE_ID` in a Rider is the Lead's workspace id. A Rider pane reports its resolved harness as `display_agent` before `agent.start`, so the sidebar never shows the internal agent name.
- Herdr does not persist per-pane env across a restart. A restored Rider is recognized as a Worker by its Mount cwd and Task pane (ADR 0007 layer 2), as before.
- Mount release still stops every process running in the Mount, including a User shell that `cd`'d there from another tab.
