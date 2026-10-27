---
name: posse-unslop
description: "Removes AI tells from prose in any language, with catalogs for English and Vietnamese. Use when you write or edit docs, a README, PR text, a commit message, a Report, a Brief, a skill, or a message to the User."
---

# posse unslop

Lead and Worker skill. It owns the slop catalogs; `posse-technical-writing` applies them.

Scope: every prose artifact and every Lead message to the User, in the language it is written in. In a branch, only the prose the branch added or changed, as in `posse-deslop`. Quotes from the User stay exactly as written.

1. **Apply the core** to every sentence in scope.
2. **Apply the catalog** for the text's language. For a language without one, apply that language's own equivalents of the core and record new tells as a `friction:` line for `posse-better-each-day`.
3. **Reread** once as the reader. Keep the meaning and the intended tone.
   Done when no sentence in scope matches the core or the catalog.

## Core (any language)

- Cut words that do no work. Use the plain everyday word.
- One idea per sentence. Name the actor: who does what.
- No filler, hedging, chatbot openers or closers, sycophancy or generic conclusions.
- Say what a thing does, or give the number, instead of how it feels.
- No em dash: end the sentence or use a comma.
- No decorative emoji. Bold only what the reader must not miss. Sentence-case headings.
- Code identifiers, commands, flags and paths stay unchanged and untranslated, in code font.
- One name per thing. Do not rotate synonyms for variety.

## English

- **AI vocabulary:** additionally, crucial, delve, enhance, foster, garner, intricate, interplay, landscape, pivotal, robust, seamless, showcase, tapestry, testament, underscore, vibrant. Use the plain word or cut.
- **Inflated verbs:** "serves as", "stands as", "boasts" for "is" or "has"; "utilize", "leverage", "facilitate", "perform" for "use", "help", "do".
- **Shapes:** "not just X, but Y"; forced groups of three; "from X to Y" with no real scale; trailing "-ing" clauses ("ensuring...", "highlighting..."); "Experts say" without a named source.
- **Filler:** "in order to", "due to the fact that", "it is important to note that", "could potentially". Write "to", "because", nothing, "may".
- **Abstract metaphor nouns:** substrate, vector, surface (as in "API surface"), primitive, north star, flywheel, paradigm, ratchet. Name the concrete thing.
- **Mannered prose:** aphorisms, personified code, figurative verbs ("rides along"). Say it literally.
- **Over-compression:** dropped articles, arrows, verbless fragments. Write whole sentences.
- **Chat tells:** "Great question!", "Certainly!", "I hope this helps", "Let me know if...". Delete.
- **Typography:** straight quotes; no bold label that restates its line ("**Speed:** Speed improved").

## Vietnamese

- **Openers and closers:** "Chắc chắn rồi!", "Tuyệt vời!", "Câu hỏi hay!", "Dưới đây là...", "Hy vọng điều này hữu ích", "Nếu cần gì thêm, cứ hỏi nhé". Delete; start with the answer.
- **"một cách" + adjective:** "chạy một cách nhanh chóng" is "chạy nhanh". Drop "một cách", or give the number.
- **Inflated importance:** "đóng vai trò quan trọng/then chốt", "là chìa khóa", "vô cùng quan trọng". Say what the thing does.
- **Stock frames:** "không chỉ... mà còn...", "nhằm đảm bảo", "trong bối cảnh", "trong thời đại ngày nay", "Tóm lại", "Nhìn chung". State the point; "nhằm đảm bảo X" is "để X".
- **Inflated words:** "toàn diện", "mạnh mẽ", "liền mạch", "tối ưu", "nâng cao", "hành trình" used as praise. Name the fact.
- **Passive calques:** "file được đọc bởi loader" is "loader đọc file". Keep "được" or "bị" only when the actor is unknown.
- **"thực hiện", "tiến hành"** before a verb or verbal noun: "thực hiện kiểm tra" is "kiểm tra".
- **Hedging:** "có thể sẽ", "có lẽ là", "về cơ bản". Commit or give the condition.
- **Mixed English:** use the common Vietnamese word where one exists ("xác nhận", not "confirm"; "cập nhật", not "update"). Terms with no common Vietnamese word (commit, branch, PR) and code identifiers stay in English.
