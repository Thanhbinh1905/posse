---
name: posse-spec
description: "Turns a done lock card into a lean spec the User approves area by area, then publishes it as one parent issue. Use when the User wants to write, draft or plan a spec for a locked idea before it is split into tickets."
---

# posse spec

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

The input is a lock card with status `done` in `~/.posse/projects/<project>/drafts/<topic>.md`. Without one, offer the `posse-sharpen` skill and stop. Write the spec into the same draft below the lock card, with its own status line (`in-progress` or `done`).

1. **Gate.** If the lock card needs one PR with an obvious approach, say Spec is not needed and offer Build directly.
   Done when the work needs more than one PR or a non-obvious approach.
2. **Map.** Write the spec yourself from the conversation and the lock card. Only when mapping the affected code needs heavy reading, first start one read-only Research Task to map the affected areas, then write from that map.
   Done when you can name every affected area.
3. **Write** these sections, in order. Length scales with stakes; small work fits one page; never pad.
   - **Problem** and **Solution**, 2-3 sentences each.
   - **Changes by area**: by module or behavior, never file paths.
   - **Decisions**: only choices that two independently built units could make incompatibly, that are non-obvious, and that are a real trade-off. Record the decision, not a rationale essay. Everything else goes to Deferred.
   - **Testing**: the highest seam and the fewest seams that prove done_when.
   - **Deferred / not-do**: mandatory; carries the lock card's Cut list.
   For a one-way door (schema, public API, file format), propose two structurally different options and let the User choose. Otherwise offer no alternatives.
   Done when every section is filled or explicitly empty.
4. **Approve by area.** The User approves or strikes each area; that is the only review. A strike that changes direction sends the idea back to `posse-sharpen`.
   Done when every remaining area is approved and the User locks the spec; then set the spec status to `done`.
5. **Publish** the spec as one parent issue labelled `ready-for-agent` with the forge CLI, and record its URL in the draft.
   Done when the issue URL is in the draft.

The User decides the next stage. Offer the `posse-tickets` skill, or Build when the spec is one ticket, in one line; never start a Worker yourself.
