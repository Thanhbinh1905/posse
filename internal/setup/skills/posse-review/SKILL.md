---
name: posse-review
description: "Reviews a Change against its spec and the repository's standards on a fixed P0-P3 scale, one pattern per root cause. Use when you are a Review Round Worker, or a Review Brief names posse-review."
---

# posse review

Worker skill for a Review Task. If `posse _context` says you are a Lead, leave review to Posse's review loop.

It complements the generated Review Round instructions (findings schema, delta scope, ledger) and never overrides them.

1. **Spec axis.** Read the Change Brief, its `ticket:` issue and the parent spec issue it points to, with all comments. Check that every done_when has evidence, and that nothing on the spec's Deferred / not-do list was built.
   Done when each done_when item is proven or flagged, and the Deferred list is checked.
2. **Standards axis.**
   - Correctness.
   - Test evidence as `posse-tdd` requires: red-before with its quoted failure, green-after, and a real-path run, or a stated reason for no test.
   - Hygiene: no slop or needless comments in added lines, no cleanup outside touched hunks.
   - Prose added in docs or PR text follows `posse-technical-writing` and `posse-unslop`.
   - The repository guidance in `review/guidance/`.
   Done when the delta was checked against each item.
3. **Grade** each finding on the fixed scale:
   - P0: broken behavior, data loss or a security hole.
   - P1: wrong against the spec or done_when, Deferred work built, or missing required test evidence.
   - P2: maintainability.
   - P3: nit.
   Done when every finding has one severity.
4. **Group.** One pattern per root cause, with every located instance. Keep P3 nits few. A finding outside the delta takes origin `pre_existing`.
   Done when no two patterns share a root cause.

One Reviewer per round: run no multi-reviewer or adversarial panel.
