// Loads the Lead Pi extension with a stub Pi API and exercises the
// posse_research tool against the fake posse named by POSSE_FAKE_BIN.
import { readFile } from "node:fs/promises";
import { spawn } from "node:child_process";
import vm from "node:vm";

const source = (await readFile(process.env.POSSE_PI_EXTENSION, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('"posse"; // POSSE_EXECUTABLE', JSON.stringify(process.env.POSSE_FAKE_BIN))
  .replace("export default function (pi)", "globalThis.start = function (pi)");
const context = vm.createContext({ spawn, console, setTimeout, clearTimeout, setInterval, clearInterval, Text: class {}, AssistantMessageComponent: class {}, ToolExecutionComponent: class {} });
vm.runInContext(source, context);

function load() {
  const tools = new Map();
  const handlers = new Map();
  const pi = {
    registerTool(tool) { tools.set(tool.name, tool); },
    registerCommand() {},
    registerMessageRenderer() {},
    sendMessage() {},
    on(name, handler) { handlers.set(name, handler); },
  };
  context.start(pi);
  return { tool: tools.get("posse_research"), handlers };
}

const ctx = { sessionManager: { getSessionId: () => "session-1" } };
const results = {};
let instance = load();
const answer = await instance.tool.execute("call-1", { task: "map the store", scope: "/repo" }, undefined, undefined, ctx);
results.answer = answer.content[0].text;
results.details = answer.details;

// A reloaded extension repeating the call uses the same key.
instance = load();
await instance.tool.execute("call-1", { task: "map the store", scope: "/repo" }, undefined, undefined, ctx);

try {
  await instance.tool.execute("call-2", { task: "no scope" }, undefined, undefined, ctx);
} catch (error) {
  results.missingScope = String(error.message);
}

// Abort cancels exactly the attempt this call started.
process.env.FAKE_POSSE_WAIT = "hang";
const controller = new AbortController();
const pending = instance.tool.execute("call-3", { task: "long", scope: "/repo" }, controller.signal, undefined, ctx).catch((error) => String(error.message));
await new Promise((resolve) => setTimeout(resolve, 300));
controller.abort();
// Shutdown cancels what this Pi process still owns.
await instance.handlers.get("session_shutdown")?.({});
process.env.FAKE_POSSE_WAIT = "";
results.aborted = await Promise.race([pending, new Promise((resolve) => setTimeout(() => resolve("still waiting"), 5000))]);
console.log(JSON.stringify(results));
process.exit(0);
