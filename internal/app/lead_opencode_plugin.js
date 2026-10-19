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
  let watcher;
  let timer;
  let stopped = false;
  let sending = false;
  let prompting = false;
  let blocked = false;
  let backoff = 1_000;
  const children = new Map();

  const schedule = (delay) => {
    if (stopped) return;
    clearTimeout(timer);
    timer = setTimeout(watch, delay);
    timer.unref?.();
  };

  const spawnOwned = (args, options) => {
    const current = spawn(posse, args, options);
    let retire;
    const closed = new Promise((resolve) => {
      let retired = false;
      retire = () => {
        if (retired) return;
        retired = true;
        children.delete(current);
        resolve();
      };
    });
    children.set(current, { closed, retire });
    current.once("error", retire);
    current.once("close", retire);
    return current;
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
    if (stopped) return resolve(false);
    const args = ["watch", "--json", "--receipt", delivery.delivery_id,
      "--receipt-outcome", outcome];
    if (delivery.owner_token) args.push("--receipt-token", delivery.owner_token);
    let current;
    try {
      current = spawnOwned(args, { stdio: ["ignore", "ignore", "ignore"] });
    } catch {
      return resolve(false);
    }
    current.once("error", () => resolve(false));
    current.once("close", (code) => resolve(code === 0));
    if (stopped) {
      current.kill();
      resolve(false);
    }
  });

  const surfaceUncertainty = (warning) => {
    if (stopped) return;
    blocked = true;
    console.error("Posse OpenCode Notice delivery uncertainty:", warning);
    client.tui?.showToast?.({ message: warning, variant: "warning" });
  };

  const retry = () => {
    schedule(backoff);
    backoff = Math.min(backoff * 2, maxBackoff);
  };

  const watch = () => {
    if (stopped || blocked || watcher || pending || !session || (sending && !prompting)) return;
    const args = ["watch", "--json", "--quiet-routine", "--handoff", "--destination", `opencode:${session}`];
    let current;
    try {
      current = spawnOwned(args, { stdio: ["ignore", "pipe", "ignore"] });
    } catch (error) {
      console.error("Posse OpenCode Notice: could not start watch", error);
      retry();
      return;
    }
    watcher = current;
    let output = "";
    current.stdout.on("data", (chunk) => { output += chunk; });
    current.on("error", () => {});
    current.on("close", (code) => {
      if (watcher === current) watcher = undefined;
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
        console.error("Posse OpenCode Notice: invalid watch result", error);
        retry();
        return;
      }
      if (result.state === "uncertain") {
        const delivery = result.delivery;
        const destination = delivery?.destination ?? "";
        const separator = destination.indexOf(":");
        const priorSession = separator < 0 ? "" : destination.slice(separator + 1);
        if (destination.startsWith("opencode:") && priorSession && priorSession !== session) {
          void (async () => {
            const priorReceipt = await sessionHasReceipt(delivery, priorSession);
            if (stopped) return;
            if (priorReceipt !== undefined) {
              const outcome = priorReceipt ? "accepted" : "rejected";
              if (await settleReceipt(delivery, outcome)) {
                if (stopped) return;
                backoff = 1_000;
                schedule(0);
                return;
              }
            }
            surfaceUncertainty(result.warning ?? "A Notice delivery has an ambiguous receipt. Inspect this session before retrying.");
          })();
          return;
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
      if (pending) {
        // The receipt is durable, but accepting another in-memory batch would
        // exceed the one-batch coalescing bound. Release it for a later read.
        void settleReceipt(delivery, "rejected").then((released) => {
          if (!stopped && !released) surfaceUncertainty(`OpenCode could not release excess Notice batch ${delivery.batch_id}.`);
        });
        return;
      }
      pending = { delivery, notices, wake: result.wake, lowkey: result.lowkey, session };
      flush();
    });
  };

  const rejectPending = async () => {
    if (!pending) return true;
    const batch = pending;
    pending = undefined;
    return settleReceipt(batch.delivery, "rejected");
  };

  const flush = () => {
    if (!pending || !session || !idle || sending || stopped || blocked) return;
    const batch = pending;
    pending = undefined;
    sending = true;
    void (async () => {
      let nextWatch = false;
      try {
        if (batch.session !== session) {
          const released = await settleReceipt(batch.delivery, "rejected");
          if (stopped) return;
          if (released) {
            nextWatch = true;
            retry();
          } else {
            surfaceUncertainty(`OpenCode could not release Notice batch ${batch.delivery.batch_id} after the session changed.`);
          }
          return;
        }

        const priorReceipt = await sessionHasReceipt(batch.delivery, batch.session);
        if (stopped) return;
        if (priorReceipt === undefined) {
          const recorded = await settleReceipt(batch.delivery, "uncertain");
          if (stopped) return;
          if (recorded) surfaceUncertainty(`OpenCode could not inspect its session history for Notice batch ${batch.delivery.batch_id}.`);
          else retry();
          return;
        }
        if (priorReceipt) {
          const accepted = await settleReceipt(batch.delivery, "accepted");
          if (stopped) return;
          if (accepted) {
            backoff = 1_000;
            nextWatch = true;
          } else retry();
          return;
        }
        if (batch.session !== session) {
          const released = await settleReceipt(batch.delivery, "rejected");
          if (stopped) return;
          if (released) {
            nextWatch = true;
            retry();
          } else {
            surfaceUncertainty(`OpenCode could not release Notice batch ${batch.delivery.batch_id} after the session changed.`);
          }
          return;
        }
        if (!idle) {
          pending = batch;
          return;
        }

        const marker = `Posse delivery receipt: ${batch.delivery.delivery_id}\nPosse batch receipt: ${batch.delivery.batch_id}`;
        let promptError;
        prompting = true;
        try {
          idle = false;
          const prompt = client.session.prompt({
            path: { id: batch.session }, query: { directory },
            body: { parts: [{ type: "text", text: `${batch.wake}\n\n${marker}` }] },
          });
          // Collect at most one additional receipt-backed batch while this
          // session.prompt turn is running. Delivery still waits for session.idle.
          schedule(0);
          const response = await prompt;
          if (response.error) promptError = new Error(JSON.stringify(response.error));
        } catch (error) {
          promptError = error;
        } finally {
          prompting = false;
        }

        if (stopped) return;
        const receipt = await sessionHasReceipt(batch.delivery, batch.session);
        if (stopped) return;
        const outcome = receipt === true ? "accepted" : receipt === false && promptError ? "rejected" : "uncertain";
        if (promptError) console.error("Posse OpenCode Notice: session delivery failed", promptError);
        const recorded = await settleReceipt(batch.delivery, outcome);
        if (stopped) return;
        if (!recorded) {
          if (outcome !== "accepted" && !(await rejectPending()) && !stopped) {
            surfaceUncertainty(`OpenCode could not release its queued Notice batch while receipt ${batch.delivery.batch_id} remained unsettled.`);
            return;
          }
          retry();
          return;
        }
        if (outcome === "uncertain") {
          if (!(await rejectPending()) && !stopped) {
            surfaceUncertainty(`OpenCode could not release its queued Notice batch after batch ${batch.delivery.batch_id} became uncertain.`);
            return;
          }
          surfaceUncertainty(`OpenCode could not determine whether Notice batch ${batch.delivery.batch_id} reached this session.`);
          return;
        }
        if (outcome === "rejected") {
          if (!(await rejectPending()) && !stopped) {
            surfaceUncertainty(`OpenCode could not release its queued Notice batch after batch ${batch.delivery.batch_id} was rejected.`);
            return;
          }
          // A definite prompt rejection leaves the session idle and the exact
          // batch is reclaimed from Posse before another prompt is attempted.
          if (session === batch.session) idle = true;
          retry();
          return;
        }
        backoff = 1_000;
        nextWatch = true;
      } finally {
        sending = false;
        if (stopped) return;
        if (idle && pending) flush();
        else if (nextWatch && !pending) schedule(0);
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
      if (stopped) return;
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
        schedule(0);
      }
    },
    dispose: async () => {
      stopped = true;
      clearTimeout(timer);
      const owned = [...children.entries()];
      for (const [child] of owned) child.kill();
      await Promise.all(owned.map(([, state]) => state.closed));
      pending = undefined;
      watcher = undefined;
    },
  };
};
