---
name: posse-deslop
description: "Removes AI slop from the lines a Worker's branch added or changed, and from nothing else. Use when you are a Worker about to publish, before posse-no-comments."
---

# posse deslop

Worker skill. If `posse _context` says you are a Lead, name it in a Brief instead; a Lead never edits code.

Run it before every publish, then follow the `posse-no-comments` skill.

1. **Scope**, the User's explicit rule: only lines this branch added or changed, from `git diff <base>...HEAD`. Leave code and comments outside those hunks as they are, even in the same file. Slop seen there becomes a `friction:` note in your done Signal, not a fix.
   Done when you know every touched hunk.
2. **Remove** from those lines:
   - defensive checks, try/catch or error wrapping that is abnormal for a trusted path;
   - casts or type escapes that only silence a type error;
   - nesting that early returns would flatten;
   - dead code;
   - anything inconsistent with the surrounding file's idiom.
   Done when no touched line matches the list.
3. **Rerun the tests.** Behavior stays unchanged.
   Done when they pass as before.
