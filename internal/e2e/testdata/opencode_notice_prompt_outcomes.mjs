import { readFile } from "node:fs/promises";
import { spawn as realSpawn } from "node:child_process";
import vm from "node:vm";

const mode = process.env.OPENCODE_OUTCOME_MODE;
const binary = process.env.OPENCODE_OUTCOME_BIN;
const pluginPath = `${process.env.OPENCODE_OUTCOME_SOURCE}/internal/app/lead_opencode_plugin.js`;
const leadFile = process.env.OPENCODE_OUTCOME_LEAD_FILE;
const messages = [];
const settlements = [];
const deliveryIDs = [];
let promptCalls = 0;
let messageQueries = 0;
let toastCount = 0;
let plugin;
let resolveFinished;
const finished = new Promise((resolve) => { resolveFinished = resolve; });

const source = (await readFile(pluginPath, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('const posse = "posse";', `const posse = ${JSON.stringify(binary)};`)
  .replace('const leadFile = "lead.md";', `const leadFile = ${JSON.stringify(leadFile)};`)
  .replace("export const PosseLead =", "globalThis.start =");

function maybeFinish() {
  const outcomes = settlements.filter((entry) => entry.code === 0).map((entry) => entry.outcome);
  if (mode === "rejected" && promptCalls === 2 && outcomes.includes("rejected") && outcomes.includes("accepted")) {
    resolveFinished({ complete: true });
  } else if (mode === "accepted-error" && promptCalls === 1 && outcomes.includes("accepted")) {
    resolveFinished({ complete: true });
  } else if (mode === "ambiguous" && promptCalls === 1 && outcomes.includes("uncertain") && toastCount === 1) {
    resolveFinished({ complete: true });
  }
}

function spawn(command, args, options) {
  const child = realSpawn(command, args, options);
  if (args.includes("--receipt-outcome")) {
    const outcome = args[args.indexOf("--receipt-outcome") + 1];
    child.once("close", (code) => {
      settlements.push({ outcome, code });
      maybeFinish();
    });
  }
  return child;
}

const context = vm.createContext({ spawn, readFile, console, setTimeout, clearTimeout });
vm.runInContext(source, context);
const client = {
  session: {
    messages: async () => {
      messageQueries++;
      if (mode === "ambiguous" && messageQueries > 1) return { error: { message: "history unavailable" } };
      return { data: messages };
    },
    prompt: async (request) => {
      promptCalls++;
      const text = request.body.parts.map((part) => part.text ?? "").join("\n");
      const deliveryID = text.match(/Posse delivery receipt: ([a-f0-9]+)/)?.[1] ?? "";
      deliveryIDs.push(deliveryID);
      if (mode === "rejected" && promptCalls === 1) throw new Error("session.prompt rejected the request");
      messages.push({ parts: request.body.parts });
      if (mode === "accepted-error") throw new Error("session.prompt failed after the message was persisted");
      if (mode === "ambiguous") throw new Error("session.prompt failed after an ambiguous acceptance");
      await plugin.event({ event: { type: "session.idle", properties: { sessionID: "outcome-session" } } });
      return { data: {} };
    },
  },
  tui: { showToast() { toastCount++; maybeFinish(); } },
};

plugin = await context.start({ client, directory: process.cwd() });
await plugin.event({ event: { type: "session.idle", properties: { sessionID: "outcome-session" } } });
let timeout;
const result = await Promise.race([
  finished,
  new Promise((resolve) => { timeout = setTimeout(() => resolve({ timeout: true }), 15000); }),
]);
clearTimeout(timeout);
await plugin.dispose();
process.stdout.write(JSON.stringify({ result, promptCalls, settlements, deliveryIDs, messageQueries, toastCount }) + "\n");
if (!result.complete) throw new Error(`OpenCode prompt outcome ${mode} did not settle`);
if (mode === "rejected" && (deliveryIDs.length !== 2 || deliveryIDs[0] !== deliveryIDs[1])) {
  throw new Error("known prompt rejection did not retry the same stable batch");
}
if (mode !== "rejected" && promptCalls !== 1) throw new Error("ambiguous or accepted delivery was prompted more than once");
