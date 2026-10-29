# 37. Worker-facing artifacts are English, and long instructions are files

`identity.lead.language` selected both the language the Lead uses with the User and the prose of Briefs, `posse send` instructions, reviews, Reports and other artifacts. With `language = "vi"`, the Lead sent Workers Vietnamese instructions as long multi-line messages pasted into their panes. The User reported this (2026-10-29):

> lead gửi msg không phải tiếng anh đến worker, điều mà trong adr đã không cho phéps, và nó đáng lẽ nên được đưa vào 1 file md trước khi gửi

`identity.lead.language` now applies to User messages only. Briefs, `posse send` instructions, reviews, decisions, Reports, commit messages, Signal notes and summaries, and other artifacts are always in neutral English. There is no separate artifact-language key: Workers, reviewers and forge readers share one language, and a Worker never has to translate its instructions. User quotations stay exact in any language.

`posse send <task> --file <path.md>` copies the file to `projects/<name>/tasks/<task>/instructions/<n>.md` and delivers one line that names the copy. The Worker reads a stable file instead of a long pasted message, and the Task directory keeps every instruction. The Lead guidance requires `--file` for any instruction longer than a few lines. `--file` cannot be combined with a message argument or `--interrupt`; an interrupt carries a short redirect.

Admitted plans still record `prose_language`, now always `English`. Relaunching a Task admitted with another language writes the English instruction.
