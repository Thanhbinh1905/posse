---
name: posse-board
description: "Opens the Project board in Lavish: every Task and ready-for-agent issue on one keyboard-first screen, where the User starts a Ready issue with one confirmation. Use when the User asks for the board, a kanban, or one view of all work."
---

# posse board

Lead skill. If `posse _context` says you are a Worker, follow your Brief instead.

`board.html` and `refresh.sh` sit beside this file. The board is fixed: never write or edit its HTML; only refresh its data.

1. **Check Lavish.** If `command -v lavish-axi` finds nothing, tell the User the board needs Lavish, give `npm install -g lavish-axi` (https://github.com/kunchenguid/lavish-axi), and stop.
   Done when `lavish-axi` is on PATH.
2. **Refresh** from the Project's repository: `sh <this skill's directory>/refresh.sh ~/.posse/projects/<project>/board` (under `$POSSE_HOME` when set). It copies `board.html` to `index.html` when it changed, writes `posse.js` from `posse --full --json` and `issues.js` from the `ready-for-agent` issues, and prints the page path. The page rereads both every 3 seconds.
   Done when the script printed the page path.
3. **Open** it with `lavish-axi <page>` and keep exactly one `lavish-axi poll <page>` running as a background command your harness tracks and wakes you from (Claude Code: `run_in_background`). Without such a facility, tell the User the board is view-only in this session and skip polling.
   Done when the page is open and one poll runs.
4. **On every wake**, for any reason, run the refresh again, then check the poll. When it returned, read only its `prompts` block, handle each prompt, and start one new poll with `--agent-reply "<one line>"`. When the User ended the session, stop polling and never reopen the board uninvited.
   Done when the data is fresh and one poll runs again, or the session ended.
5. **Start** for a prompt tagged `posse-board-start`: its data names the issue. The confirmation on the board is the User's approval, so do not ask again. Write a one-ticket Brief by the Brief step of the `posse-build` skill, with `ticket: <issue>`, `review: quick` and `after:` for open blockers, and an `## Intent` block quoting the prompt text verbatim as `User's words (confirmed on posse-board):`. Start it with `posse start --queue`, then refresh: the card turns into the Task and the activity line shows the start. If the issue already has an open Task, or a Brief cannot be written, reply with the reason through `--agent-reply` instead.
   Done when the Task exists or the User has the reason.

Any other prompt is ordinary User feedback: act on it as if typed in the Lead pane.
