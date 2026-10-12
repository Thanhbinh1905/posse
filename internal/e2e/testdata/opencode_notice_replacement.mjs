import { readFile } from "node:fs/promises";
import { spawn as realSpawn } from "node:child_process";
import vm from "node:vm";

const posse = process.env.POSSE_E2E_POSSE_BIN;
const extensionPath = process.env.POSSE_E2E_OPENCODE_PLUGIN;
const leadFile = process.env.POSSE_E2E_LEAD_FILE;
const deliveryID = process.env.POSSE_E2E_OLD_DELIVERY_ID;
const batchID = process.env.POSSE_E2E_OLD_BATCH_ID;
const outcomes = [];
const queriedSessions = [];
let promptCalls = 0;
const source = (await readFile(extensionPath, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('const posse = "posse";', `const posse = ${JSON.stringify(posse)};`)
  .replace('const leadFile = "lead.md";', `const leadFile = ${JSON.stringify(leadFile)};`)
  .replace("export const PosseLead =", "globalThis.start =");

function spawn(command, args, options) {
  const outcome = args.includes("--receipt-outcome") ? args[args.indexOf("--receipt-outcome") + 1] : undefined;
  const child = realSpawn(command, args, options);
  if (outcome) child.once("close", (code) => outcomes.push(code === 0 ? outcome : `failed:${outcome}`));
  return child;
}

const context = vm.createContext({
  spawn,
  readFile,
  console,
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
});
vm.runInContext(source, context);
const client = {
  session: {
    messages: async ({ path }) => {
      queriedSessions.push(path.id);
      if (path.id === "old-session") {
        return { data: [{ parts: [{ type: "text", text: `Posse delivery receipt: ${deliveryID}\nPosse batch receipt: ${batchID}` }] }] };
      }
      return { data: [] };
    },
    prompt: async () => {
      promptCalls++;
      throw new Error("replacement must not receive a duplicate prompt");
    },
  },
  tui: { showToast() {} },
};
const plugin = await context.start({ client, directory: process.cwd() });
await plugin.event({ event: { type: "session.idle", properties: { sessionID: "new-session" } } });
const deadline = Date.now() + 5000;
while (!outcomes.includes("accepted") && Date.now() < deadline) {
  await new Promise((resolve) => setTimeout(resolve, 20));
}
await plugin.dispose();
if (outcomes.length !== 1 || outcomes[0] !== "accepted") {
  throw new Error(`replacement did not resolve prior-session evidence: ${outcomes.join(",")}`);
}
if (!queriedSessions.includes("old-session") || promptCalls !== 0) {
  throw new Error(`replacement queried=${queriedSessions.join(",")} prompts=${promptCalls}`);
}
process.stdout.write(JSON.stringify({ outcomes, queriedSessions, promptCalls }) + "\n");
