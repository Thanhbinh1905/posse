// Generated per Lead start. Loaded only into that OpenCode process through
// OPENCODE_CONFIG_CONTENT; no plugin is installed in the user's config.
import { spawn } from "node:child_process";
import { readFile } from "node:fs/promises";

const posse = "posse"; // POSSE_EXECUTABLE
const leadFile = "lead.md"; // POSSE_LEAD_FILE
const maxBackoff = 60_000;

export const PosseLead = async ({ client, directory }) => {
  const instructions = await readFile(leadFile, "utf8");
  let session;
  let idle = false;
  let pending;
  let child;
  let timer;
  let stopped = false;
  let sending = false;
  let blocked = false;
  let backoff = 1_000;

  const schedule = (delay) => {
    clearTimeout(timer);
    timer = setTimeout(watch, delay);
    timer.unref?.();
  };

  const sessionHasReceipt = async (delivery, sessionID = session) => {
    if (typeof client.session?.messages !== "function" || !sessionID) return undefined;
    try {
      const response = await client.session.messages({ path: { id: sessionID }, query: { directory } });
      if (response?.error) return undefined;
      const messages = response?.data ?? response;
      if (!Array.isArray(messages)) return undefined;
      const markers = [`Posse delivery receipt: ${delivery.delivery_id}`, `Posse batch receipt: ${delivery.batch_id}`];
      return messages.some((message) => (message.parts ?? []).some((part) =>
        part.type === "text" && typeof part.text === "string" && markers.some((marker) => part.text.includes(marker))));
    } catch {
      return undefined;
    }
  };

  const settleReceipt = (delivery, outcome) => new Promise((resolve) => {
    const args = ["lookout", "--json", "--receipt", delivery.delivery_id,
      "--receipt-outcome", outcome];
    if (delivery.owner_token) args.push("--receipt-token", delivery.owner_token);
    const current = spawn(posse, args, { stdio: ["ignore", "ignore", "ignore"] });
    child = current;
    current.on("error", () => {});
    current.on("close", (code) => {
      if (child === current) child = undefined;
      resolve(code === 0);
    });
  });

  const surfaceUncertainty = (warning) => {
    blocked = true;
    console.error("Posse OpenCode Notice delivery uncertainty:", warning);
    client.tui?.showToast?.({ message: warning, variant: "warning" });
  };

  const retry = () => {
    schedule(backoff);
    backoff = Math.min(backoff * 2, maxBackoff);
  };

  const watch = () => {
    if (stopped || blocked || child || pending || !session) return;
    const args = ["lookout", "--json", "--quiet-routine", "--handoff", "--destination", `opencode:${session}`];
    const current = spawn(posse, args, { stdio: ["ignore", "pipe", "ignore"] });
    child = current;
    let output = "";
    current.stdout.on("data", (chunk) => { output += chunk; });
    current.on("error", () => {});
    current.on("close", async (code) => {
      if (child !== current) return;
      child = undefined;
      if (stopped) return;
      if (code !== 0) {
        retry();
        return;
      }
      let result;
      try {
        result = JSON.parse(output);
      } catch (error) {
        // A printed receipt remains durable, so the next query can recover it.
        console.error("Posse OpenCode Notice: invalid lookout result", error);
        retry();
        return;
      }
      if (result.state === "uncertain") {
        const delivery = result.delivery;
        const destination = delivery?.destination ?? "";
        const separator = destination.indexOf(":");
        const priorSession = separator < 0 ? "" : destination.slice(separator + 1);
        if (destination.startsWith("opencode:") && priorSession && priorSession !== session) {
          const priorReceipt = await sessionHasReceipt(delivery, priorSession);
          if (priorReceipt !== undefined) {
            const outcome = priorReceipt ? "accepted" : "rejected";
            if (await settleReceipt(delivery, outcome)) {
              backoff = 1_000;
              schedule(0);
              return;
            }
          }
        }
        surfaceUncertainty(result.warning ?? "A Notice delivery has an ambiguous receipt. Inspect this session before retrying.");
        return;
      }
      const notices = result.notices ?? [];
      if (notices.length === 0) {
        backoff = 1_000;
        schedule(0);
        return;
      }
      const delivery = result.delivery;
      if (!delivery?.delivery_id || !delivery?.batch_id || !delivery?.owner_token ||
          !notices.every((notice) => Number.isSafeInteger(notice.id) && notice.id > 0) ||
          typeof result.wake !== "string" || !result.wake.startsWith("[posse | Posse -> Lead ")) {
        console.error("Posse OpenCode Notice: invalid receipt-backed batch");
        retry();
        return;
      }
      pending = { delivery, notices, wake: result.wake, lowkey: result.lowkey, session };
      flush();
    });
  };

  // session.prompt waits for the result and reports errors, unlike promptAsync
  // which accepts a request even if its background turn later fails. Only send
  // after session.idle so an active model turn is never interrupted. Neither
  // path writes to the TUI composer.
  const flush = () => {
    if (!pending || !session || !idle || sending || stopped) return;
    const batch = pending;
    if (batch.session !== session) {
      pending = undefined;
      void settleReceipt(batch.delivery, "uncertain").then(() => surfaceUncertainty(
        `OpenCode changed sessions before Notice batch ${batch.delivery.batch_id} could be confirmed.`));
      return;
    }
    sending = true;
    void (async () => {
      try {
        const priorReceipt = await sessionHasReceipt(batch.delivery);
        if (priorReceipt === undefined) {
          const recorded = await settleReceipt(batch.delivery, "uncertain");
          pending = undefined;
          if (recorded) surfaceUncertainty(`OpenCode could not inspect its session history for Notice batch ${batch.delivery.batch_id}.`);
          else retry();
          return;
        }
        if (priorReceipt) {
          // Reconciliation consumes only a persisted receipt; it does not
          // start a model turn or change the session's idle state.
          const accepted = await settleReceipt(batch.delivery, "accepted");
          pending = undefined;
          if (accepted) {
            backoff = 1_000;
            schedule(0);
          } else retry();
          return;
        }

        const marker = `Posse delivery receipt: ${batch.delivery.delivery_id}\nPosse batch receipt: ${batch.delivery.batch_id}`;
        let promptError;
        try {
          if (!idle) return;
          idle = false;
          const response = await client.session.prompt({
            path: { id: session }, query: { directory },
            body: { parts: [{ type: "text", text: `${batch.wake}\n\n${marker}` }] },
          });
          if (response.error) promptError = new Error(JSON.stringify(response.error));
        } catch (error) {
          promptError = error;
        }

        const receipt = await sessionHasReceipt(batch.delivery);
        let outcome;
        if (receipt === true) outcome = "accepted";
        else if (receipt === false && promptError) outcome = "rejected";
        else outcome = "uncertain";
        if (promptError) console.error("Posse OpenCode Notice: session delivery failed", promptError);
        const recorded = await settleReceipt(batch.delivery, outcome);
        pending = undefined;
        if (!recorded) {
          retry();
          return;
        }
        if (outcome === "uncertain") {
          surfaceUncertainty(`OpenCode could not determine whether Notice batch ${batch.delivery.batch_id} reached this session.`);
          return;
        }
        if (outcome === "accepted") backoff = 1_000;
        schedule(outcome === "accepted" ? 0 : backoff);
        if (outcome === "rejected") backoff = Math.min(backoff * 2, maxBackoff);
      } finally {
        sending = false;
      }
    })();
  };

  schedule(0);
  return {
    "experimental.chat.system.transform": async (_input, output) => {
      // Append to OpenCode's system stack; do not replace its provider prompt.
      output.system.push(instructions);
    },
    event: async ({ event }) => {
      const id = event.properties?.sessionID;
      if (!id) return;
      if (event.type === "session.idle") {
        session = id;
        idle = true;
        blocked = false;
        flush();
        schedule(0);
      } else if (event.type === "session.status" && event.properties.status?.type === "busy") {
        session = id;
        idle = false;
      }
    },
    dispose: async () => {
      stopped = true;
      clearTimeout(timer);
      child?.kill();
    },
  };
};
