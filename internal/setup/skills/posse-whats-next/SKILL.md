---
name: posse-whats-next
description: "Reads Posse state and gives the User one next step that names the skill to use. Use when the User asks what to do now, what comes next, where to start, or which Posse skill fits."
---

# posse what's next

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

Facts are your job; decisions are the User's. Label each claim seen, inferred or guess.

1. **Read state.** Run `posse`, then list unfinished drafts: files under `<drafts>/` (`<drafts>` is the `pending.drafts` path that `posse lead` prints) whose status line says `in-progress`.
   Done when you know what waits on the User, what is running and which drafts are open.
2. **Locate the User.** Infer where they are from state and the chat. Ask one question only when two next steps remain equally likely.
   Done when one stage fits, or the User answered.
3. **Give one next step.** Name one action and the skill that does it, with a one-line reason. Something waiting on the User comes first.
   - Something waiting on the User or time away: `posse-catch-up`.
   - Wants to understand code or a past choice: `posse-enlighten-me`; a broad question that needs sources: `posse-research`.
   - A thin idea: `posse-brainstorming`; an idea to lock for a spec: `posse-sharpen`; a done lock card to specify: `posse-spec`; a locked spec to break into tickets: `posse-tickets`; approved tickets or an approved fix to build: `posse-build`.
   - Something broken: `posse-diagnose`.
   - A batch of open or old issues to check and group for Build: `posse-triage`.
   - A clear change: write a Brief and run `posse start --name <two-to-four-words>` after the User's yes.
   - Recurring friction or end of day: `posse-better-each-day`.
   - An open draft: "continue <topic>" resumes it in the skill that wrote it.
   Done when the reply names exactly one step.

Show this whole map only when the User asks for it. Never start the next skill or a Worker yourself; the User picks.
