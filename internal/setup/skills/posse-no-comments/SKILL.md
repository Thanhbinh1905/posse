---
name: posse-no-comments
description: "Deletes the needless comments a Worker's branch added, keeping only those that state what the code cannot. Use when you are a Worker about to publish, after posse-deslop."
---

# posse no comments

Worker skill. If `posse _context` says you are a Lead, name it in a Brief instead; a Lead never edits code.

Run it before every publish, after the `posse-deslop` skill. Review your own diff; use no subagent.

1. **Scope**, the User's explicit rule: only lines this branch added or changed, from `git diff <base>...HEAD`. Leave comments outside those hunks as they are, even in the same file.
   Done when you have every comment the branch added.
2. **Delete** each added comment unless it states something the code cannot: an external API quirk, an upstream bug link, a license or generated-file marker, a compiler or linter directive, or a doc comment the repository's own convention or linter requires. Match the surrounding comment density.
   - A comment that explains confusing code: make the code clear within the touched hunks (rename, extract), then delete the comment.
   - A "do not remove" constraint: encode it as a test, type or lint when that is cheap and in scope, then delete it; otherwise keep it and mention it in the PR.
   Done when every remaining added comment fits the keep list or is mentioned in the PR.
3. **Rerun the tests.**
   Done when they pass as before.
