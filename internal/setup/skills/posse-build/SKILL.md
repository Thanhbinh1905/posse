---
name: posse-build
description: "Turns approved tickets or an approved diagnose fix into thin Change Briefs the User approves as one set, starts them, then proves the spec end to end once they merge. Use when the User wants to build, implement or start Workers on approved tickets or a fix."
---

# posse build

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

The input is the approved `ready-for-agent` sub-issues of a spec issue from `posse-tickets`, or one fix the User approved after `posse-diagnose`. Without either, offer the `posse-tickets` or `posse-diagnose` skill and stop.

1. **Brief** each ticket thinly; point to the ticket and parent spec issue instead of copying them.
   - A conventional title and `ticket: <n>`.
   - `done_when`: the ticket's acceptance criteria (a diagnose fix: the repro turning green), plus "Lines added or changed in this branch carry no AI slop or needless comments, and no code or comment outside the touched hunks was changed for cleanup." The Review Round checks done_when, so hygiene becomes a gate.
   - `after: [tN:landed]` for each `Blocked by: #n`.
   - The Profile its Dispatch Rules select, and a `--name` of 2-4 words, at most 24 characters.
   - A body that says "Follow the `posse-tdd` skill" with its entry: bug, plus the repro command and root cause, or feature.
   Done when every ticket has a Brief.
2. **Approve.** Show all Briefs as one table: name, Profile, blocked by. One User approval covers every ticket in it.
   Done when the User approves the set.
3. **Start** each with `posse start --queue` in dependency order, so blockers have Task IDs before their dependants name them in `after:`. Posse admits each when its prerequisites land and the Pool has room; add no concurrency limit of your own.
   Done when every Brief has a Task.
4. **Deliver** when every ticket's PR merged: run the spec's done_when (a fix: the repro) end to end on the updated target branch, on the User's surface, and show the evidence. Only then close the parent spec issue, if any, with a short outcome comment.
   Done when the User has the evidence and the spec issue is closed.

A Worker that finds the spec wrong asks instead of changing direction: raise its question with `posse ask`. The User decides; a direction change goes back to `posse-spec`.

Review runs through Posse's review loop and the `posse-review` skill; CI and merge through Posse.

After Deliver, offer a release when the repository has its own release skill, and the `posse-better-each-day` skill when friction was recorded.
