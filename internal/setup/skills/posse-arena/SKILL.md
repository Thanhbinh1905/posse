---
name: posse-arena
description: "Runs 2-3 independent design candidates for a large one-way-door decision, scores them against a rubric and proposes one direction to the User. Use when a spec's one-way door is too large for posse-spec's two options."
---

# posse arena

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

Candidates work independently; they never debate, because a debate drifts toward the most confident voice or a compromise. For a smaller decision, use the two options in the `posse-spec` skill instead.

1. **Ask first.** Tell the User the decision, why it is a one-way door, and how many Workers the arena starts.
   Done when the User says yes.
2. **Frame.** The lock card is the shared contract. Write a rubric of 3-6 gradeable criteria and keep it out of the Brief; candidates see the task, not the rubric.
   Done when the rubric and one Research Brief exist.
3. **Fan out.** Start 2-3 read-only Research Tasks from that same Brief, each on a Profile from a different model family. Each returns one design and the alternatives it rejected, with reasons.
   Done when every candidate Report is in, or a dropout is noted.
4. **Judge.** Read every candidate in full and score it per criterion, not on overall feel.
   - Pick as the base the design a maintainer can extend most easily; graft at most one or two ideas from the others by hand.
   - Converging candidates: ship the consensus.
   - Wildly diverging candidates: the lock card was under-specified. Go back to the `posse-sharpen` skill instead of averaging.
   Done when one direction carries its scores, base, grafts and what was rejected.
5. **Decide.** The User picks the direction; record it in the spec's Decisions section and continue in `posse-spec`.
   Done when the decision is in the spec draft.
