---
name: posse-enlighten-me
description: "Explains how code works or why it is shaped that way, from code, docs and history, with each claim labelled seen, inferred or guess. Use when the User asks how X works, why X is like this, where X lives, or which option to pick, or asks to explain or break down a PR, the architecture or a spec (ELI5, giải thích), before you start reading code to answer."
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

## Break down

Use this mode when the User asks to explain, break down or ELI5 a PR, the architecture or a spec, or says "giải thích". Read from the conversation why the User asks and what they already know, and answer in the User's language.

- **Sources.** A PR: `posse diff <task>`, the PR body, the Brief and the review findings. The architecture: `CONTEXT.md`, `docs/spec.md` and the ADR index. A spec: the draft or the spec issue.
- **Layers.** 1: the answer in one sentence. 2: a plain story with one concrete example and no code identifiers. 3: the mechanism. 4: where it lives. Stop after layer 2 and offer the next layer.
- **A PR** gets four blocks instead: the intent, quoted verbatim from the ticket; the broad strokes; one slice per concern, not per file; then the periphery.
- When the User says they did not understand, explain it again more simply, with a different example.

A "how does X work" question keeps the senior-engineer pitch above.
