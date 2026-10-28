---
name: posse-better-each-day
description: "Turns recorded friction into upstream Posse issues and proposed skill edits for the User to approve. Use when the User asks what should improve, wants a retro, or ends the day."
---

# posse better each day

Lead skill, except the capture line below. A Worker follows its Brief and adds that line to its done Signal note.

## Capture (cheap, immediately)

- **Worker:** when Posse or a skill got in the way, end the done Signal note with one `friction: <what happened, what you expected>` line. When a skill caused it, write `friction: [skill <name>] <what happened, what you expected>`.
- **Lead:** append one dated line to the draft `<drafts>/better-each-day.md`, with status line `in-progress` (`<drafts>` is the `pending.drafts` path that `posse lead` prints):
  - when the User corrects you: the correction in the User's words and what you did before it;
  - when a skill misfires: the skill, and whether it triggered when it should not have or did not trigger when it should have.

## Consolidate (when the User asks, or at the end of a day)

1. **Collect** since the last consolidation: the `friction:` lines from done Signal notes (`posse show <task>`), the lines in the draft, Posse errors seen in this session, and review patterns that recur across two or more Change Tasks (`posse review <task>`).
   Done when every item has its evidence: Task, quote or error code.
2. **Sort each item:**
   - **Posse bug or missing workflow:** one occurrence with evidence is enough. Search open and closed issues in Thanhbinh1905/posse for a duplicate; comment on a match with the new evidence, otherwise file an issue with what happened, what was expected, impact and repro.
   - **Skill guidance gap:** counts only after it happened twice. A misfire usually needs a description fix. A recurring review pattern shows a weak Worker skill (`posse-tdd`, `posse-deslop`, `posse-no-comments`, `posse-unslop`). Prefer a structural fix (a check, a test, CLI behaviour) and propose that; otherwise propose the smallest skill edit as a diff, removing or tightening lines before adding any. Never edit a skill yourself: after the User approves, a Posse-shipped skill changes through a Change Task in Thanhbinh1905/posse, and a repository-local skill (for example under `.agents/skills/`) through a Change Task in that repository.
   - **Anything else** (a one-off, a task mistake): drop it.
   Done when each item is filed, proposed or dropped with a reason.
3. **Report** in the `posse-catch-up` shape: what waits on the User first (each proposed edit with your pick, evidence and risk), then the issues filed or updated, then one next step. Mark the consolidated draft lines as handled.
   Done when the User has every proposal and its evidence in one reply.
