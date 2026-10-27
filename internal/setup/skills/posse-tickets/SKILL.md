---
name: posse-tickets
description: "Groups a locked spec into the fewest tickets, one Worker per pattern or area, that the User approves, then publishes them as sub-issues of the spec issue. Use when the User wants to break down, split or ticket a spec before Build."
---

# posse tickets

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

The input is a locked parent spec issue from `posse-spec`, read with all its comments. Without one, offer the `posse-spec` skill and stop. Write the breakdown into the spec's draft `~/.posse/projects/<project>/drafts/<topic>.md` below the spec, with its own status line (`in-progress` or `done`).

1. **Gate.** If the spec yields one ticket, say Tickets is not needed and offer a Brief directly.
   Done when the spec needs more than one Worker.
2. **Group first.** One ticket is one Worker and one PR, verifiable on its own. Never let several Workers do the same kind of work in different places.
   - Same-pattern changes (same fix, same recurring defect, same refactor shape) form one ticket however many places they touch.
   - Work in one area or category that needs the same context forms one ticket.
   Done when every spec area sits in a group.
3. **Split second**, only with a reason stated in the ticket: a different kind of work needing different context; a blocking edge; or too large for one Worker session, then expand-contract batches (add the new form beside the old, migrate callers in batches, remove the old form last).
   Done when every split carries its reason.
4. **Check** convergence and readiness: merge any two tickets that share a pattern, or touch the same area without a blocking edge. Every spec area has a ticket, and every done_when has a ticket that proves it.
   Done when no merge applies and nothing is uncovered.
5. **Approve.** Show the breakdown as a numbered list: title, what it delivers, blocked by, split reason. The User approves; that is the only review.
   Done when the User approves; then set the breakdown status to `done`.
6. **Publish** with the forge CLI in dependency order, as sub-issues of the spec issue, each labelled `ready-for-agent` with a `Blocked by: #n` line. Record the issue URLs in the draft. Never close the parent issue or modify its body.
   Done when every ticket URL is in the draft.

The User decides the next stage. Offer the `posse-build` skill in one line; never start a Worker yourself.
