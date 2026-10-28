---
name: posse-brainstorming
description: "Develops a thin idea as a creative partner, one prompt per turn, then narrows it to 1-3 directions. Use when the User wants to develop, expand or explore an idea, or an idea is too thin to sharpen."
---

# posse brainstorming

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

Optional. When an idea looks thin, offer this skill in one line; run it only on the User's yes or request. Keep a draft at `<drafts>/<topic>.md` (`<drafts>` is the `pending.drafts` path that `posse lead` prints): a status line (`in-progress` or `done`), then one line per idea, tagged with who said it. "Continue <topic>" resumes from the draft.

1. **Diverge, one prompt per turn.** Pick one lens and ask one open prompt. The User answers first; then add 1-2 sparks of your own that differ from theirs, and hand back. Never offer a menu of ideas or lenses.
   - "Let me think" switches you to prompts only, no sparks. Never take over the ideating.
   - Lenses: Job to Be Done, Laddering ("and what would that give you?"), Assumption Reversal, Kill the Crown Jewel, One Feature Only, Trait Transfer, Backcasting, Utopia vs Dystopia, Worst Possible Idea, Ship in 60 Minutes.
   Done when every turn holds one prompt and the User spoke first in the lens.
2. **Switch and stop.** After a turn with no new idea, switch to a lens far from the current one. Push past the User's first "I'm out" once, with a new lens. Stop after two lenses in a row bring only variants, or after 3-4 lenses. There is no idea-count target.
   Done when a stop rule fired.
3. **Converge with one move.** State the criteria first (fit to the goal, effort, how easily it can be cut) and let the User adjust them. Cluster the ideas, place each cluster on impact versus effort, and name 1-3 directions.
   Done when the User picks a direction or stops.
4. **Hand off.** Set the draft status to `done` and suggest the `posse-sharpen` skill for the chosen direction.
   Done when the draft is saved and the User has the next step.
