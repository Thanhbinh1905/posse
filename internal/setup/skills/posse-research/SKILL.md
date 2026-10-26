---
name: posse-research
description: "Splits a question into 2-4 source directions and runs each as a read-only Research Task on primary sources, then sums up the findings. Use when the User asks to research, investigate, dig into or compare something with sources, before you start searching yourself."
---

# posse research

This skill has a Lead part and a Worker part. Run `posse _context` to find yours.

## Lead

1. **Keep small questions.** A question one or two lookups can answer is not research: answer it yourself, labelling each claim seen, inferred or guess.
   Done when the answer is given, or the question needs more than a few sources.
2. **Split by source.** Propose 2-4 directions, each owning one kind of primary source (code and history, official docs, specs or standards, issues and PRs, papers), with the question it answers. The User adds or removes directions.
   Done when the User says yes to a list.
3. **Start Workers.** For each direction write a Research Brief that says "Follow the `posse-research` skill", the question, the sources it owns and the Report length limit. Run `posse start --name <two-to-four-words>`; see `posse start --help`.
   Done when every approved direction has a Task.
4. **Synthesize** when the Reports arrive: a short chat summary with the answer first, agreements, conflicts between sources, and what nobody found. Keep each label and citation.
   Done when the User can decide from the summary without opening a Report.

## Worker

Your Brief owns the question and sources; this part sets the method.

1. Use primary sources only: source code, official docs, specs, first-party APIs, original papers. Follow a secondary write-up back to its source, or drop it.
2. Cite every claim (`path:line`, commit, URL or section) and label it seen, inferred or guess.
3. Record each search that found nothing, with the query and where you looked.
4. Keep the Report within the Brief's limit, or 150 lines if it names none: answer first, evidence after.

Done when every claim in the Report has a citation and a label, and the Report states what was not found.
