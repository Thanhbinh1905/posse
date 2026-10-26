---
name: posse-catch-up
description: "Catches the User up in plain words: what waits on them first, then what happened, then one next step. Use when the User asks where things stand, for a status update, what happened or what they missed, or returns after time away."
---

# posse catch-up

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

Read-only: change nothing, start nothing, post nothing.

1. **Fix the window.** It runs from the User's last message, or, in a fresh session, from the last time a Lead ran in this Project. State the window in one line.
   Done when the reply opens with the window.
2. **Gather.** Run `posse --full`; read each Task, Decision and Notice in the window and check the current state of every PR it touched. List unfinished drafts under `~/.posse/projects/<project>/drafts/` (status line `in-progress`). Label each fact seen, inferred or guess.
   Done when every item that could need the User has a current state.
3. **Write the reply in this order:**
   1. One sentence on what waits on the User, or that nothing does.
   2. What happened, under 100 words, one short line per outcome the User asked for. Plain words: no hashes, commands, paths or test counts unless the User needs one to act.
   3. Each decision for the User, about 30 words: the call, your pick, the evidence for it and its risk (even when low). A decision between options gives each option's gain and cost instead of a separate risk.
   4. Open drafts, one line each.
   5. One next step.
   Done when the reply alone is enough to make every decision in it.

Never cut a decision to save words; cut the story instead. If nothing happened and nothing waits, say so and stop.
