---
name: posse-technical-writing
description: "Writes or edits technical prose in any language so a tired engineer understands it on the first read. Use when you write or review docs, a README, a spec, PR text, a commit message, a Report, a Brief or a skill."
---

# posse technical writing

Lead and Worker skill. Apply the `posse-unslop` skill to everything this skill touches; it owns the slop catalogs.

Scope: every prose artifact, in the language it is written in. In a branch, only the prose the branch added or changed, as in `posse-deslop`. Quotes from the User stay exactly as written.

1. **Pick the mode** of the document (below) before writing.
2. **Write** to the core and to the rules for the text's language. When a rule makes a sentence worse, fix the sentence another way.
3. **Apply `posse-unslop`**, then reread once as the reader.
   Done when the text holds one mode and passes both skills.

## Core (any language)

- The codebase is the word list: write the real symbol, file, flag or command, unchanged and untranslated. One name per thing.
- Use words a developer says aloud. Define a named pattern the first time it appears; invent no jargon.
- One idea per sentence. Name the actor. Put a condition or warning before the step it guards.
- Common case first, exceptions after. Numbered lists for sequences, bullets otherwise.
- Headings state the point, in sentence case. Link text says where the link goes.
- Be specific: "a column rename fails the build", not "schema changes can cause issues". Mix sentence lengths.
- Make every count or claim true at the commit that lands it.
- A PR description or commit message is a briefing a reviewer reads in under a minute: what changed, why, how it was verified. Link logs; do not paste them.

## Modes (Diataxis)

One document, one mode; split and link rather than mix.

- **Tutorial** (learning by doing): every step shows a visible result and says what the reader should see.
- **How-to** (a goal at work): steps only, titled by the task; link background instead of including it.
- **Reference** (lookup): dry, complete, mirroring the structure of the thing described; no instruction or opinion.
- **Explanation** (why): one bounded topic with its context, constraints and trade-offs; the only mode with opinion.

## English

- Address the reader as "you", present tense. Instructions are commands: "Run `posse doctor`."
- Active voice; passive only when the actor is unknown or beside the point.
- Never "simply", "easy" or "just" in a procedure, and no "please".
- Split instructions over about 20 words and other sentences over about 25.
- Keep articles: "remove the backup file", not "remove backup file". Keep "only" and "not" next to the word they change.
- Break noun stacks: "the script that checks the import budget", not "the import budget check script".
- Every "it", "this" and "they" points at one noun; repeat the noun when in doubt.
- No slashes for "or", no "(s)" plurals, no "etc.", no Latin abbreviations, no idioms. Serial comma.

## Vietnamese

- Address the reader as "bạn" throughout, or write instructions without a pronoun ("Chạy `posse doctor`."). Do not switch forms.
- Every sentence has a clear subject and verb; split sentences chained with commas or "và".
- Avoid "thì", "là", "mà" used as filler between clauses.
- Keep one Vietnamese term per concept for the whole document, and keep English technical terms the team already uses.
