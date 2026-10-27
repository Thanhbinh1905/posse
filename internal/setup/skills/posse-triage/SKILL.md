---
name: posse-triage
description: "Checks that open issues are still real on the default branch, sorts out fixed, duplicate and deferred ones, and groups the rest into Briefs for Build. Use when the User wants to triage, sweep or clean up open issues, or hands over a batch of old issues."
---

# posse triage

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

The User starts it; never schedule it. The input is the open issues of the Project's forge, or the subset the User names: a label, a search or a list.

1. **Read** every issue with all its comments. An owner comment may defer an issue that still carries `ready-for-agent`.
   Done when every issue is read.
2. **Verify** each issue on the current default branch, cheapest evidence first, and label each verdict seen, inferred or guess:
   - merged PRs or commits since the issue was filed that close, reference or touch what it describes (`gh` search, `git log --since=<created> -- <paths>`);
   - whether the code, messages or excerpts the issue cites still exist;
   - its repro command, only when cheap and read-only.
   Heavier checks go to one read-only Research Task per group; do not run them yourself.
   Done when every issue has a verdict and its evidence.
3. **Sort** into fixed (with the PR or commit), duplicate (with the surviving issue, among open and recently closed issues), deferred by the owner, needs-info, and still valid.
   Done when every issue sits in one bucket.
4. **Group** the still-valid issues by the `posse-tickets` rule: same pattern or same area in one group, split only with a stated reason. Each group is one candidate Brief for `posse-build`.
   Done when every still-valid issue is in a group.
5. **Approve.** Show one table of proposed actions: close with evidence, mark duplicate, relabel, groups to build. Never close, relabel or dispatch before the User approves the batch.
   Done when the User approves.
6. **Apply** with the forge CLI, commenting the evidence on every issue you close.
   Done when the forge matches the approved table.

Offer the `posse-build` skill for the approved groups in one line.
