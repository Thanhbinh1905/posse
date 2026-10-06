import { readFile } from "node:fs/promises";
import { spawn as realSpawn } from "node:child_process";
import vm from "node:vm";

const extensionPath = process.env.POSSE_E2E_PI_EXTENSION;
const posse = process.env.POSSE_E2E_POSSE_BIN;
const deliveryID = process.env.POSSE_E2E_OLD_DELIVERY_ID;
const batchID = process.env.POSSE_E2E_OLD_BATCH_ID;
const outcomes = [];
const messages = [];
const entries = [{
  type: "custom_message",
  customType: "posse-notices",
  details: { delivery_id: deliveryID, batch_id: batchID },
}];
const handlers = new Map();
const source = (await readFile(extensionPath, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('"posse"; // POSSE_EXECUTABLE', JSON.stringify(posse))
  .replace("export default function (pi)", "globalThis.start = function (pi)");

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
  sendMessage(message) { messages.push(message); },
};
context.start(pi);
const ui = { notify() {}, setWidget() {}, setWorkingVisible() {}, setHiddenThinkingLabel() {} };
const sessionManager = { getSessionId: () => "new-session", getEntries: () => entries };
await handlers.get("session_start")({}, { ui, sessionManager });
const deadline = Date.now() + 5000;
while (!outcomes.includes("accepted") && Date.now() < deadline) {
  await new Promise((resolve) => setTimeout(resolve, 20));
}
await handlers.get("session_shutdown")();
if (outcomes.length !== 1 || outcomes[0] !== "accepted") {
  throw new Error(`Pi did not accept the matching prior-session batch record: ${outcomes.join(",")}`);
}
if (messages.some((message) => message.details?.type === "notice")) {
  throw new Error("Pi replayed a batch already recorded in the replacement session");
}
process.stdout.write(JSON.stringify({ outcomes, messages: messages.length }) + "\n");
