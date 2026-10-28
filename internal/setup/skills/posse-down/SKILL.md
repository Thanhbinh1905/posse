---
name: posse-down
description: "Shut a Posse Project down from the Lead chat: preview the plan, get the User's ok, then tear down every Task, stop the Watch and close the Lead."
disable-model-invocation: true
---

# posse down

The CLI is the source of truth. Follow its actual output.

1. **Plan.** Run `posse down --plan` in the Project folder. It changes nothing. Show the User every Task with its state, the Tasks whose work will be lost, the PRs that stay open, and the pending Decisions and Notices that stay.
   Done when the User has seen the whole plan.
2. **Wait for the User's ok.** Stop here until the User approves this plan in their own words. Anything other than a clear ok means no shutdown. If the plan says a Task is merging, `--execute` refuses; tell the User and stop.
   Done when the User approved, or the shutdown is called off.
3. **Execute.** Run `posse down --execute --user-approved "<User's words>"` with the User's approval quoted exactly. It tears down every Task, cancels queued ones, discards unmerged work under that quote, stops the Watch, marks the Project down and closes this Lead's tab last, so its result may never reach you.
   Done when the command returns `state: down` or this session ends.

If `--execute` returns an error, report it to the User and stop. Do not imitate shutdown with Signals, pane closes or database edits, and never add a force flag. Decisions, Notices, pushed branches and open PRs are kept.

To restart, the User runs `posse up` in the same folder.
