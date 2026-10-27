---
name: posse-diagnose
description: "Turns a reported bug into a reproduced root cause and a proposed fix for the User to approve. Use when the User says something is broken, failing, flaky, slow, stuck or wrong, before you start investigating."
---

# posse diagnose

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

1. **Find the owner.** Check whether the bug comes from an unmerged Task: run `posse --full` and compare the symptom with each live Task's branch and PR. If one owns it, steer that Worker with `posse send` (see `posse send --help`) and stop here.
   Done when an owning Task was steered, or none owns the bug.
2. **Restate** the symptom in the User's words, with what they expected and where they saw it.
   Done when the restatement quotes their report or the User confirms it.
3. **Reproduce on the User's surface** (the CLI, UI or flow they used) if it takes a few commands. Label each finding seen, inferred or guess.
   Done when one command or step sequence fails now, or a few attempts did not get there.
4. **No repro yet: propose a Diagnose Worker.** Start it after the User's yes, with `posse start --name <two-to-four-words>`. Its Brief asks for:
   - one red-capable repro command that fails now and will pass after the fix;
   - 3-5 ranked, falsifiable hypotheses, each with the check that would disprove it, shown before any fix;
   - a Report, not a code change.
   Done when step 3 reproduced the bug, the Worker is started, or the User declined.
5. **Close** with root cause (labelled), the repro command and the proposed fix in one message. A fix runs only after the User's yes, through the `posse-build` skill, as a Change Brief whose done_when is the repro turning green.
   Done when the User has the three items and the decision.

Gotcha: a symptom only the User's environment shows needs their exact steps and versions before step 3, not after.
