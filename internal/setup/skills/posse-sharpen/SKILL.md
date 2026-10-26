---
name: posse-sharpen
description: "Locks an idea or a raw issue into a lean lock card with a Cut list, ready for a spec, through short rounds of questions. Use when the User wants to lock, pin down, scope or triage an idea or issue before it is specified."
---

# posse sharpen

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

The output is a lock card for the Spec stage, not a Worker Brief. Keep it in the draft `~/.posse/projects/<project>/drafts/<topic>.md`: a status line (`in-progress` or `done`), then one line per decision. "Continue <topic>" resumes from the draft.

1. **Gate.** Read the input: the chat, or an issue with all its comments (an owner comment may defer it). A complete ready-for-agent spec ticket needs no sharpening; say so and stop. Look up every fact yourself and prototype what is observable; ask the User only for decisions.
   Done when you know which lock card fields are still open.
2. **Ask in rounds.** The lock card floor is goal, not-do, a falsifiable done_when and hard constraints, each confirmed by the User. Fill what you can confidently and show it in the read-back instead of asking.
   - Ask a question only if its plausible answers would change the lock card or the Cut list and it does not depend on another open question. Highest impact first, one idea per question, never padding.
   - At most 4 questions per round, usually 2-3. Ask goal and done_when open-ended first. Every other question carries one recommendation with a one-line reason, worded so "yes" accepts it, defaulting to the smaller scope.
   - If the User asks for one question at a time, ask one per turn under the same limits.
   Done when no candidate question passes the test, or a round changed nothing.
3. **Lean gate.** Keep an item in Do only if the goal would not be met without it. Anything unresolved goes to Cut. No new config, abstraction or layer without a concrete current blocker.
   Done when every Do item survived the question.
4. **Read back** the whole lock card (goal, Do, not-do, done_when, hard constraints, stated assumptions) and the Cut list in one message, and wait for explicit confirmation.
   Done when the User confirms; then set the draft status to `done`.

Ceiling: 3 rounds. At the ceiling never start a fourth: lock what is settled, turn each open item into a stated assumption or a Cut, and propose splitting the idea into several small lock cards.

The User decides the next stage. Offer it in one line; never start a spec or a Worker yourself.
