---
name: posse-tdd
description: "Implements a fix or feature test-first and proves it on the real user path, with red-before and green-after evidence for the PR. Use when you are a Worker changing code for a Change Brief, or a Brief names posse-tdd."
---

# posse tdd

Worker skill. If `posse _context` says you are a Lead, name it in a Brief instead; a Lead never edits code.

Your Brief names the entry: bug or feature.

1. **Bug entry.** The Brief carries the repro command and the diagnosed root cause. Turn the repro into the narrowest failing test at the closest existing test seam. Run it and quote the failure content that proves it measures the bug, not just the assertion line. Fix the root cause with the smallest change and rerun to green. Re-investigate only if the diagnosed cause proves wrong, and report that.
   Done when the test failed for the bug's reason and now passes.
2. **Feature entry.** Use the seams from the spec's Testing section. One behavior at a time: one failing test through the public interface, the least code to pass it, repeat. Refactor only while green. Write each test just before the code it drives, never a batch first.
   Done when every acceptance criterion has a test that went red, then green.
3. **Prove it on the real path.** Run the change once the way a user would (the CLI command, UI or flow), not only the tests. For a UI change, attach a screenshot with `posse publish --proof`.
   Done when the real-path run shows the expected result.
4. **Report** in `posse publish --verify`: the red-before check with its quoted failure, the green-after run and the real-path run.
   Done when the PR carries all three.

No test is better than a bad test: one built mostly on mocks, implementation details, timing or global state, or expensive infrastructure for a small change. Then use the closest executable check and state why in the PR.

Never change a test to match a wrong implementation, or weaken an assertion without a stated behavior change.

If the ticket or spec proves wrong, signal `needs-decision` with the evidence instead of changing direction.
