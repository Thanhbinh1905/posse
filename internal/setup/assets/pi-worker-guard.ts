// Installed by `posse setup`; `posse setup --uninstall` removes it.
// In a posse Worker it refuses bash commands and file writes that could change
// the User's Herdr session or posse home, through `posse _guard`. Anywhere else
// it allows everything.
// @ts-nocheck

import { spawnSync } from "node:child_process";
import { homedir } from "node:os";
import { resolve } from "node:path";

const posse = {{POSSE_BINARY}};

const fileTools = { write: "Write", edit: "Edit" };

export default function (pi) {
	pi.on("tool_call", async (event) => {
		let toolInput;
		if (event.toolName === "bash") {
			const command = event.input?.command;
			if (typeof command !== "string" || command.trim() === "") return;
			toolInput = { command };
		} else if (fileTools[event.toolName]) {
			let path = event.input?.path;
			if (typeof path !== "string" || path === "") return;
			// pi resolves the path the same way before it writes.
			path = path.replace(/^@/, "");
			if (path === "~" || path.startsWith("~/")) path = homedir() + path.slice(1);
			toolInput = { file_path: resolve(process.cwd(), path) };
		} else {
			return;
		}
		const input = JSON.stringify({ hook_event_name: "PreToolUse", tool_name: fileTools[event.toolName] ?? "Bash", cwd: process.cwd(), tool_input: toolInput });
		const result = spawnSync(posse, ["_guard"], { input, encoding: "utf8", timeout: 5_000 });
		if (result.status === 2) {
			return { block: true, reason: (result.stderr || "posse refused this command from a Worker").trim() };
		}
	});
}
