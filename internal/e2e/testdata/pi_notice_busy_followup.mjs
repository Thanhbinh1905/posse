import { readFile } from "node:fs/promises";
import { spawn as realSpawn } from "node:child_process";
import vm from "node:vm";

const extensionPath = process.env.POSSE_E2E_PI_EXTENSION;
const posse = process.env.POSSE_E2E_POSSE_BIN;
const source = (await readFile(extensionPath, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('"posse"; // POSSE_EXECUTABLE', JSON.stringify(posse))
  .replace("export default function (pi)", "globalThis.start = function (pi)");
const handlers = new Map();
const followUps = [];
const entries = [];
const outcomes = [];
let agentActive = false;
let notificationReceived;
const received = new Promise((resolve) => { notificationReceived = resolve; });

function spawn(command, args, options) {
  const outcome = args.includes("--receipt-outcome") ? args[args.indexOf("--receipt-outcome") + 1] : undefined;
  const child = realSpawn(command, args, options);
  if (outcome) child.once("close", (code) => outcomes.push(code === 0 ? outcome : `failed:${outcome}`));
  return child;
}

const context = vm.createContext({
  spawn,
  console,
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  Text: class {},
  AssistantMessageComponent: undefined,
  ToolExecutionComponent: undefined,
});
vm.runInContext(source, context);

const pi = {
  registerCommand() {},
  registerMessageRenderer() {},
  on(name, handler) { handlers.set(name, handler); },
  sendMessage(message, options) {
    if (message.details?.type === "notice") {
      if (options?.deliverAs !== "followUp" || options?.triggerTurn !== true) {
        throw new Error("Notice was not queued as a follow-up");
      }
      followUps.push(message);
      notificationReceived();
    } else {
      followUps.push(message);
    }
  },
};

context.start(pi);
const ui = {
  notify() {},
  setWidget() {},
  setWorkingVisible() {},
  setHiddenThinkingLabel() {},
};
const sessionManager = {
  getSessionId: () => "busy-session",
  getEntries: () => entries,
};
await handlers.get("session_start")({}, { ui, sessionManager });
agentActive = true;
handlers.get("agent_start")({}, { ui });

await Promise.race([
  received,
  new Promise((_, reject) => setTimeout(() => reject(new Error("Pi did not receive a Notice")), 5000)),
]);
await new Promise((resolve) => setTimeout(resolve, 150));
if (outcomes.length !== 0) {
  throw new Error(`queued follow-up settled before session consumption: ${outcomes.join(",")}`);
}

const notice = followUps.find((message) => message.details?.type === "notice");
if (!notice) throw new Error("queued follow-up disappeared");
// Pi persists a busy-session follow-up only when the run consumes it. Model
// that lifecycle ordering explicitly without credentials or a provider call.
entries.push({
  type: "custom_message",
  customType: notice.customType,
  details: notice.details,
});
followUps.splice(followUps.indexOf(notice), 1);
agentActive = false;
handlers.get("agent_settled")({}, { ui, isIdle: () => true });

const deadline = Date.now() + 5000;
while (!outcomes.includes("accepted") && Date.now() < deadline) {
  await new Promise((resolve) => setTimeout(resolve, 20));
}
await handlers.get("session_shutdown")();
if (outcomes.length !== 1 || outcomes[0] !== "accepted") {
  throw new Error(`Pi did not accept its persisted follow-up at settlement: ${outcomes.join(",")}`);
}
if (entries[0]?.details?.delivery_id !== notice.details.delivery_id) {
  throw new Error("persisted Pi session entry lost the stable delivery ID");
}
process.stdout.write(JSON.stringify({ queued: 1, persisted: entries.length, outcomes }) + "\n");
