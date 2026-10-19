import { readFile, writeFile } from "node:fs/promises";
import { spawn as realSpawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import vm from "node:vm";

const extensionPath = process.env.POSSE_E2E_PI_EXTENSION;
const posse = process.env.POSSE_E2E_POSSE_BIN;
const statePath = process.env.POSSE_E2E_LIFECYCLE_STATE;
const keepAlive = setInterval(() => {}, 1000);
const source = (await readFile(extensionPath, "utf8"))
  .replace(/^import .*;\n/gm, "")
  .replace('"posse"; // POSSE_EXECUTABLE', JSON.stringify(posse))
  .replace("export default function (pi)", "globalThis.start = function (pi)");
class AssistantMessageComponent {
  updateContent(message) {
    this.lastMessage = message;
    this.visibleContent = message.content;
    return this.visibleContent;
  }
  setHiddenThinkingLabel() {}
}
class ToolExecutionComponent {
  markExecutionStarted() {}
  render() { return ["tool row"]; }
}

const extensionSetTimeout = (callback, delay, ...args) => setTimeout(callback, Math.min(delay, 10), ...args);
const context = vm.createContext({
  spawn,
  console,
  setTimeout: extensionSetTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  Text: class {},
  AssistantMessageComponent,
  ToolExecutionComponent,
});
vm.runInContext(source, context);

const reasons = [];
const destinations = [];
const entries = [];
const outcomes = [];
const noticeMessages = [];
const attemptedNoticeMessages = [];
const healthMessages = [];
const uncertaintyMessages = [];
const noticeQueue = [];
const noticeWaiters = [];
let failWatchSpawns = 0;
let failedWatchAttempts = 0;
let failSendNotices = 0;
let activeWatches = 0;
let maxActiveWatches = 0;
let watchStarts = 0;
let delayNextKill;
let releaseDelayedKill;
let currentSession = "initial-session";

let stateWrite = Promise.resolve();
function updateState(destination, stage = "") {
  destinations.push(destination);
  stateWrite = stateWrite.then(() => writeFile(statePath, JSON.stringify({
    session: destination.slice("pi:".length),
    activeWatches,
    stage,
  })));
  return stateWrite;
}

function spawn(command, args, options) {
  const isHandoff = args[0] === "watch" && args.includes("--handoff");
  if (isHandoff && failWatchSpawns > 0) {
    failWatchSpawns--;
    failedWatchAttempts++;
    const failed = new EventEmitter();
    failed.stdout = new PassThrough();
    failed.kill = () => true;
    setImmediate(() => {
      failed.emit("error", new Error("injected spawn failure"));
      failed.emit("close", 127, null);
    });
    return failed;
  }
  const child = realSpawn(command, args, options);
  const outcomeIndex = args.indexOf("--receipt-outcome");
  if (isHandoff) {
    const destination = args[args.indexOf("--destination") + 1];
    activeWatches++;
    maxActiveWatches = Math.max(maxActiveWatches, activeWatches);
    watchStarts++;
    updateState(destination);
  }
  if (outcomeIndex >= 0) {
    const outcome = args[outcomeIndex + 1];
    child.once("close", (code) => outcomes.push(code === 0 ? outcome : `failed:${outcome}`));
  }
  const wrapped = new Proxy(child, {
    get(target, property) {
      if (property === "kill" && isHandoff) {
        return (...killArgs) => {
          if (delayNextKill) {
            delayNextKill = false;
            releaseDelayedKill = () => target.kill(...killArgs);
            return true;
          }
          return target.kill(...killArgs);
        };
      }
      if (property === "on" && isHandoff) {
        return (event, listener) => {
          if (event === "close") {
            target.once(event, (...eventArgs) => {
              activeWatches--;
              listener(...eventArgs);
            });
            return wrapped;
          }
          target.on(event, listener);
          return wrapped;
        };
      }
      const value = Reflect.get(target, property, target);
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
  return wrapped;
}

const ui = {
  notify() {},
  setWidget() {},
  setWorkingVisible() {},
  setHiddenThinkingLabel() {},
};
const sessionManager = (id) => ({
  getSessionId: () => id,
  getEntries: () => entries,
});

function nextNotice() {
  if (noticeQueue.length > 0) return Promise.resolve(noticeQueue.shift());
  return new Promise((resolve) => noticeWaiters.push(resolve));
}

function loadExtension() {
  const handlers = new Map();
  const commands = new Map();
  const pi = {
    registerCommand(name, command) { commands.set(name, command); },
    registerMessageRenderer() {},
    on(name, handler) { handlers.set(name, handler); },
    sendMessage(message, options) {
      if (message.details?.type === "delivery-error") {
        healthMessages.push(message);
        return;
      }
      if (message.details?.type === "delivery-uncertain") {
        uncertaintyMessages.push(message);
        return;
      }
      if (message.details?.type !== "notice") return;
      attemptedNoticeMessages.push(message);
      if (failSendNotices > 0) {
        failSendNotices--;
        throw new Error("injected send failure");
      }
      if (options?.deliverAs !== "followUp" || options?.triggerTurn !== true) {
        throw new Error("Notice was not queued as a follow-up");
      }
      noticeMessages.push(message);
      const waiter = noticeWaiters.shift();
      if (waiter) waiter(message);
      else noticeQueue.push(message);
    },
  };
  context.start(pi);
  return { handlers, commands };
}

async function startSession(reason, id) {
  currentSession = id;
  const instance = loadExtension();
  await instance.handlers.get("session_start")({ type: "session_start", reason }, {
    ui,
    sessionManager: sessionManager(id),
  });
  return instance;
}

async function waitFor(predicate, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error("timed out waiting for Pi session lifecycle readiness");
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

let instance = await startSession("startup", currentSession);
const originalToolStep = { role: "assistant", content: [
  { type: "thinking", thinking: "preserve reasoning" },
  { type: "text", text: "tool preamble" },
  { type: "toolCall", id: "tool-1" },
] };
const transcript = new AssistantMessageComponent();
transcript.updateContent(originalToolStep, true);
if (transcript.lastMessage !== originalToolStep || originalToolStep.content.length !== 3 || transcript.visibleContent.length !== 1 || transcript.visibleContent[0].type !== "toolCall") {
  throw new Error("lowkey changed session content or failed to hide only its presentation");
}
const toolRow = new ToolExecutionComponent();
toolRow.markExecutionStarted();
if (toolRow.render().length !== 0) throw new Error("lowkey left a tool row visible");
transcript.updateContent({ role: "assistant", content: [{ type: "text", text: "final response" }] }, true);
if (transcript.visibleContent[0]?.text !== "final response") throw new Error("lowkey hid the final response after a tool");
await waitFor(() => activeWatches === 1);
for (const [reason, session] of [
  ["reload", "initial-session"],
  ["new", "new-session"],
  ["resume", "resumed-session"],
  ["fork", "fork-session"],
]) {
  reasons.push(reason);
  delayNextKill = true;
  await instance.handlers.get("session_shutdown")({ type: "session_shutdown", reason });
  instance = await startSession(reason, session);
  if (!releaseDelayedKill) throw new Error(`session ${reason} did not stop its previous watch`);
  const release = releaseDelayedKill;
  releaseDelayedKill = undefined;
  release();
  await waitFor(() => activeWatches === 1 && destinations.at(-1) === `pi:${session}`);
}

await updateState(`pi:${currentSession}`, "ready-first-notice");
const lifecycleStarts = watchStarts;
const message = await nextNotice();
if (message.display !== false) throw new Error("lowkey Notice was visible in the transcript");
instance.handlers.get("agent_start")({ type: "agent_start" }, { ui });
entries.push({ type: "custom_message", customType: message.customType, details: message.details });
instance.handlers.get("agent_settled")({ type: "agent_settled", aborted: false }, { ui, isIdle: () => true });
await waitFor(() => outcomes.includes("accepted"));
await waitFor(() => watchStarts > lifecycleStarts);
const rearmedWatches = watchStarts - lifecycleStarts;
failSendNotices = 1;
await updateState(`pi:${currentSession}`, "ready-send-failure");
const retryMessage = await nextNotice();
if (retryMessage.display !== false) throw new Error("lowkey retry Notice was visible in the transcript");
instance.handlers.get("agent_start")({ type: "agent_start" }, { ui });
entries.push({ type: "custom_message", customType: retryMessage.customType, details: retryMessage.details });
const beforeRetryRearm = watchStarts;
instance.handlers.get("agent_settled")({ type: "agent_settled", aborted: false }, { ui, isIdle: () => true });
await waitFor(() => outcomes.filter((outcome) => outcome === "accepted").length >= 2);
await waitFor(() => watchStarts > beforeRetryRearm);
const retryAttempts = attemptedNoticeMessages.filter((attempt) => attempt.details.ids[0] === retryMessage.details.ids[0]);
const retryKeptStableID = retryAttempts.length === 2 && retryAttempts[0].details.delivery_id === retryAttempts[1].details.delivery_id && retryAttempts[0].details.batch_id === retryAttempts[1].details.batch_id;
if (!retryKeptStableID) throw new Error("known rejected send did not retry the stable batch ID");

await instance.commands.get("lowkey").handler("off", { ui });
transcript.updateContent(originalToolStep, true);
if (transcript.lastMessage !== originalToolStep || transcript.visibleContent !== originalToolStep.content || toolRow.render().length !== 1) {
  throw new Error("lowkey off did not restore the original transcript presentation");
}

await updateState(`pi:${currentSession}`, "ready-replacement");
const replacementMessage = await nextNotice();
const replacementDeliveryID = replacementMessage.details.delivery_id;
const replacementBatchID = replacementMessage.details.batch_id;
await instance.handlers.get("session_shutdown")({ type: "session_shutdown", reason: "new" });
reasons.push("new");
instance = await startSession("new", "replacement-session");
const runtime = vm.runInContext('globalThis[Symbol.for("posse:pi-notice-runtime:v1")]', context);
const replacementPendingRetained = runtime.pendingAcceptance.has(replacementDeliveryID);
await waitFor(() => uncertaintyMessages.length === 1);
const uncertainty = uncertaintyMessages[0];
const replacementUncertaintyIDs = uncertainty.details.ids;
const replacementUncertaintyKeptIDs = uncertainty.details.delivery_id === replacementDeliveryID && uncertainty.details.batch_id === replacementBatchID && JSON.stringify(replacementUncertaintyIDs) === JSON.stringify(replacementMessage.details.ids);
if (!replacementPendingRetained || !replacementUncertaintyKeptIDs) throw new Error("unconsumed replacement batch lost its recoverable receipt IDs");
await instance.handlers.get("session_shutdown")({ type: "session_shutdown", reason: "quit" });
reasons.push("quit");
await waitFor(() => activeWatches === 0);

failWatchSpawns = 5;
const failureInstance = await startSession("startup", "failure-session");
await waitFor(() => healthMessages.length === 1);
await new Promise((resolve) => setTimeout(resolve, 50));
if (failedWatchAttempts !== 5 || healthMessages.length !== 1) {
  throw new Error(`watch failure episode attempts=${failedWatchAttempts} notices=${healthMessages.length}`);
}
await failureInstance.handlers.get("session_shutdown")({ type: "session_shutdown", reason: "quit" });

process.stdout.write(JSON.stringify({
  reasons,
  destinations,
  maxActiveWatches,
  autoRearmedWatches: rearmedWatches,
  deliveredNoticeIDs: message.details.ids,
  sendFailureNoticeIDs: retryMessage.details.ids,
  sendFailureAttempts: retryAttempts.length,
  sendFailureKeptStableID: retryKeptStableID,
  replacementNoticeIDs: replacementMessage.details.ids,
  replacementDeliveryID,
  replacementBatchID,
  replacementPendingRetained,
  replacementUncertaintyIDs,
  failedWatchAttempts,
  healthEpisodes: healthMessages.length,
}) + "\n");
clearInterval(keepAlive);
