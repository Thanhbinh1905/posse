// Installed by `posse setup`; `posse setup --uninstall` removes it.
// In a posse Worker it refuses bash commands that could change the User's
// Herdr session or posse home, through `posse _guard`. Anywhere else it
// allows everything.
// @ts-nocheck

import { spawnSync } from "node:child_process";

const posse = {{POSSE_BINARY}};

export default function (pi) {
	pi.on("tool_call", async (event) => {
		if (event.toolName !== "bash") return;
		const command = event.input?.command;
		if (typeof command !== "string" || command.trim() === "") return;
		const input = JSON.stringify({ hook_event_name: "PreToolUse", tool_name: "Bash", cwd: process.cwd(), tool_input: { command } });
		const result = spawnSync(posse, ["_guard"], { input, encoding: "utf8", timeout: 5_000 });
		if (result.status === 2) {
			return { block: true, reason: (result.stderr || "posse refused this command from a Worker").trim() };
		}
	});
}
