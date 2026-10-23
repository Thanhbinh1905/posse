---
name: posse-down
description: "Safely stop a Posse Project Lead and Watch."
disable-model-invocation: true
---

# posse down

Inspect `posse --full` from the Project folder. Review Project state, Tasks, `needs_you`, undelivered Notices, and Watch state. Stop if any live Task exists in `spawning`, `working`, `needs-decision`, `blocked`, `stalled`, `done` or `merging`. Ask the User to finish, merge, accept or teardown those Tasks first.

`posse down` has no force flag. Do not imitate it with Signals, pane closes or database edits. Shutdown does not delete the registered Project, Tasks, branches, Sandboxes, Decisions or Notices. Failed or lost Tasks, pending Decisions and undelivered Notices remain, but no Lead acts on them until `posse up`.

The User must run exactly `posse down` from a User shell in this Project folder. They can open a Herdr shell pane with no agent, or exit the Lead agent so its pane becomes a shell. Never run the command yourself. Expect `state=down`, closed panes, and guidance to `Run \`posse up\` here to start the Lead again`.

To restart, the User runs `posse up` from a User shell in the same folder. Hand off now and wait for the User to run the command. Do not claim the Project is down before then.
