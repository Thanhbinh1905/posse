import { readFile, writeFile } from "node:fs/promises";
import { spawn as realSpawn } from "node:child_process";
import vm from "node:vm";

const binary = process.env.OPENCODE_BUSY_BIN;
const pluginPath = `${process.env.OPENCODE_BUSY_SOURCE}/internal/app/lead_opencode_plugin.js`;
const leadFile = process.env.OPENCODE_BUSY_LEAD_FILE;
const releasePrompt = process.env.OPENCODE_BUSY_RELEASE;
const reloadPlugin = process.env.OPENCODE_BUSY_RELOAD;
const resumedPlugin = process.env.OPENCODE_BUSY_RESUMED;
const statusFile = process.env.OPENCODE_BUSY_STATUS;
const messages = [];
const promptTargets = [];
const composerDraft = { text: "half-typed User draft" };
const settlementCodes = [];
const settlementAttempts = [];
const watchFailures = [];
const activeLookouts = new Set();
const spawnedPids = [];
const closedPids = new Set();
let promptCalls = 0;
let plugin;
let disposing = false;
let resolveFinished;
const finished = new Promise((resolve) => { resolveFinished = resolve; });

const source = (await readFile(pluginPath, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('const posse = "posse";', `const posse = ${JSON.stringify(binary)};`)
  .replace('const leadFile = "lead.md";', `const leadFile = ${JSON.stringify(leadFile)};`)
  .replace("export const PosseLead =", "globalThis.start =");

function spawn(command, args, options) {
  const child = realSpawn(command, args, options);
  if (child.pid) spawnedPids.push(child.pid);
  child.once("close", () => { if (child.pid) closedPids.add(child.pid); });
  if (args.includes("--receipt-outcome")) {
    settlementAttempts.push(args[args.indexOf("--receipt-outcome") + 1]);
    child.once("close", (code) => {
      if (code === 0) settlementCodes.push(code);
      if (settlementCodes.length >= 3 && promptCalls >= 3) resolveFinished({ complete: true });
    });
  } else if (args.includes("--handoff")) {
    activeLookouts.add(child);
    child.once("close", (code) => {
      activeLookouts.delete(child);
      if (!disposing && code !== 0) watchFailures.push(code);
    });
  }
  return child;
}

async function writeStatus(busy) {
  await writeFile(statusFile, JSON.stringify({ promptCalls, busy, settlementCodes: settlementCodes.length }));
}

async function waitForFile(path, timeout, error) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    try {
      await readFile(path);
      return;
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  throw new Error(error);
}

async function waitForActiveLookout() {
  const deadline = Date.now() + 2000;
  while (Date.now() < deadline) {
    if (activeLookouts.size > 0) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error("OpenCode did not re-arm a lookout before shutdown");
}

const context = vm.createContext({ spawn, readFile, console, setTimeout, clearTimeout });
vm.runInContext(source, context);
const client = {
  session: {
    messages: async ({ path, query }) => {
      if (path.id !== "busy-session" || query.directory !== process.cwd()) throw new Error("session history query was misrouted");
      return { data: messages };
    },
    prompt: async (request) => {
      if (request.path.id !== "busy-session" || request.query.directory !== process.cwd()) throw new Error("session.prompt was misrouted");
      promptTargets.push(request.path.id);
      promptCalls++;
      messages.push({ parts: request.body.parts });
      await writeStatus(true);
      if (promptCalls === 1) await waitForFile(releasePrompt, 15000, "test did not release the busy OpenCode prompt");
      else await plugin.event({ event: { type: "session.idle", properties: { sessionID: "busy-session" } } });
      await writeStatus(false);
      return { data: {} };
    },
  },
  tui: { showToast() {}, composer: composerDraft },
};

const firstPlugin = await context.start({ client, directory: process.cwd() });
plugin = firstPlugin;
await firstPlugin.event({ event: { type: "session.idle", properties: { sessionID: "busy-session" } } });
await waitForFile(reloadPlugin, 15000, "test did not request an OpenCode plugin reload");
await firstPlugin.dispose();
const retiredChildren = spawnedPids.every((pid) => closedPids.has(pid));
plugin = await context.start({ client, directory: process.cwd() });
await plugin.event({ event: { type: "session.status", properties: { sessionID: "busy-session", status: { type: "busy" } } } });
await writeFile(resumedPlugin, JSON.stringify({ retiredChildren }));
await waitForFile(releasePrompt, 15000, "test did not release the busy OpenCode prompt");
await plugin.event({ event: { type: "session.idle", properties: { sessionID: "busy-session" } } });
let timeout;
const result = await Promise.race([
  finished,
  new Promise((resolve) => { timeout = setTimeout(() => resolve({ timeout: true }), 20000); }),
]);
clearTimeout(timeout);
await waitForActiveLookout();
const shutdownHadLookout = activeLookouts.size > 0;
disposing = true;
await plugin.dispose();
const allChildrenRetired = spawnedPids.every((pid) => closedPids.has(pid));
process.stdout.write(JSON.stringify({ result, promptCalls, promptTargets, composerDraft: composerDraft.text, settlementCodes, settlementAttempts, watchFailures, retiredChildren, shutdownHadLookout, allChildrenRetired }) + "\n");
if (!result.complete || promptCalls !== 3 || promptTargets.some((target) => target !== "busy-session") || composerDraft.text !== "half-typed User draft" || settlementCodes.length < 3 || settlementAttempts.length !== 3 || watchFailures.length !== 0 || !retiredChildren || !shutdownHadLookout || !allChildrenRetired) {
  throw new Error("OpenCode did not observe and deliver the batch collected during the earlier busy turn");
}
