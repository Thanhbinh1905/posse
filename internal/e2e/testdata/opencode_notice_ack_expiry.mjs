import { readFile } from "node:fs/promises";
import { spawn as realSpawn } from "node:child_process";
import vm from "node:vm";

const binary = process.env.NOTICE_ACK_BIN;
const pluginPath = `${process.env.NOTICE_ACK_SOURCE}/internal/app/lead_opencode_plugin.js`;
const leadFile = process.env.NOTICE_ACK_LEAD_FILE;
const noticeID = process.env.NOTICE_ACK_ID;
const settlementCodes = [];
const watchFailures = [];
const messages = [];
let promptCalls = 0;
let plugin;
let resolveFinished;
const finished = new Promise((resolve) => { resolveFinished = resolve; });

const source = (await readFile(pluginPath, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('const posse = "posse";', `const posse = ${JSON.stringify(binary)};`)
  .replace('const leadFile = "lead.md";', `const leadFile = ${JSON.stringify(leadFile)};`)
  .replace("export const PosseLead =", "globalThis.start =");

function spawn(command, args, options) {
  const child = realSpawn(command, args, options);
  if (args.includes("--receipt-outcome")) {
    child.once("close", (code) => {
      settlementCodes.push(code);
      if (code === 0 && settlementCodes.filter((value) => value === 0).length >= 2 && promptCalls >= 2) {
        resolveFinished({ complete: true });
      }
    });
  } else if (args.includes("--handoff")) {
    child.once("close", (code) => {
      if (code !== 0) {
        watchFailures.push(code);
        if (watchFailures.length >= 2) resolveFinished({ failed: true });
      }
    });
  }
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
    messages: async () => ({ data: messages }),
    prompt: async (request) => {
      promptCalls++;
      messages.push({ parts: request.body.parts });
      if (promptCalls === 1) {
        const ack = realSpawn(binary, ["ack", noticeID, "--json"], { stdio: "ignore" });
        const code = await new Promise((resolve) => ack.once("close", resolve));
        if (code !== 0) throw new Error(`in-turn ack exited ${code}`);
        const deadline = Date.now() + 10000;
        while (Date.now() < deadline) {
          try {
            await readFile(process.env.NOTICE_ACK_RELEASE);
            break;
          } catch {
            await new Promise((resolve) => setTimeout(resolve, 10));
          }
        }
        if (Date.now() >= deadline) throw new Error("test did not release the pending OpenCode turn");
      }
      await plugin.event({ event: { type: "session.idle", properties: { sessionID: "session-1" } } });
      return { data: {} };
    },
  },
  tui: { showToast() {} },
};
plugin = await context.start({ client, directory: process.cwd() });
await plugin.event({ event: { type: "session.idle", properties: { sessionID: "session-1" } } });
let timeout;
const result = await Promise.race([
  finished,
  new Promise((resolve) => { timeout = setTimeout(() => resolve({ timeout: true }), 15000); }),
]);
clearTimeout(timeout);
await plugin.dispose();
process.stdout.write(JSON.stringify({ result, promptCalls, settlementCodes, watchFailures }) + "\n");
if (!result.complete || promptCalls !== 2 || watchFailures.length !== 0 || settlementCodes[0] === 0) {
  throw new Error("OpenCode did not reclaim its acknowledged receipt and deliver the later Notice");
}
