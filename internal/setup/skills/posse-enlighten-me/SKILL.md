---
name: posse-enlighten-me
description: "Explains how code works or why it is shaped that way, from code, docs and history, with each claim labelled seen, inferred or guess. Use when the User asks how X works, why X is like this, where X lives, or which option to pick."
---

# posse enlighten me

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

1. **Size the question.** Narrow (one function, one file, one decision): answer it yourself. Broad (a subsystem, several repositories, outside sources): propose 2-4 read-only angles in one message and run them through the `posse-research` skill after the User's yes.
   Done when the question is answered directly or the User approved the angles.
2. **Gather evidence.** Read the code, docs, ADRs, `git log`/`git blame` and the PR or issue behind each relevant commit. Use read-only research children for parallel reading. Label every claim seen (with `path:line`, commit or link), inferred, or guess.
   Done when each claim in the answer has a label and the strongest available source.
3. **Answer** in this shape, dropping sections that do not apply:
   - **Overview**: the answer in two or three sentences.
   - **Key concepts**: the terms the User needs, in the project's own vocabulary.
   - **How it works**: the runtime flow, or for a "why", the forces and decisions that shaped it.
   - **Where it lives**: the files and entry points.
   - **Gotchas**: what surprises people or breaks when changed.
   A choice between options instead gets a trade-off table (option, gain, cost) and one recommendation.
   Done when the User can act on the answer without opening the sources.

Pitch it at a senior engineer meeting the subsystem for the first time, not annotated source. Say what you could not find instead of filling the gap; the newest commit is not automatically the reason.
