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
  let requeue;
  let child;
  let timer;
  let stopped = false;
  let sending = false;
  let backoff = 1_000;

  const schedule = (delay) => {
    clearTimeout(timer);
    timer = setTimeout(watch, delay);
    timer.unref?.();
  };

  const watch = () => {
    if (stopped || child || pending) return;
    const args = ["lookout", "--json", "--quiet-routine"];
    if (requeue) args.push("--requeue", requeue);
    const current = spawn(posse, args, { stdio: ["ignore", "pipe", "ignore"] });
    child = current;
    let output = "";
    current.stdout.on("data", (chunk) => { output += chunk; });
    current.on("error", () => {});
    current.on("close", (code) => {
      if (child !== current) return;
      child = undefined;
      if (stopped) return;
      if (code === 0) {
        requeue = undefined;
        try {
          const result = JSON.parse(output);
          const notices = result.notices ?? [];
          if (notices.length) {
            const ids = notices.map((notice) => notice.id);
            if (!ids.every((id) => Number.isSafeInteger(id) && id > 0) ||
                typeof result.wake !== "string" || !result.wake.startsWith("[posse | Posse -> Lead ")) {
              throw new Error("invalid Notice envelope");
            }
            pending = { ids, wake: result.wake };
            flush();
            return;
          }
          backoff = 1_000;
          schedule(0);
          return;
        } catch (error) {
          console.error("Posse OpenCode Notice: invalid lookout result", error);
        }
      }
      schedule(backoff);
      backoff = Math.min(backoff * 2, maxBackoff);
    });
  };

  // session.prompt waits for the result and reports errors, unlike promptAsync
  // which accepts a request even if its background turn later fails. Only send
  // after session.idle so an active model turn is never interrupted. Neither
  // path writes to the TUI composer.
  const flush = () => {
    if (!pending || !session || !idle || sending || stopped) return;
    const batch = pending;
    sending = true;
    idle = false;
    void (async () => {
      try {
        const response = await client.session.prompt({
          path: { id: session }, query: { directory },
          body: { parts: [{ type: "text", text: batch.wake }] },
        });
        if (response.error) throw new Error(JSON.stringify(response.error));
        pending = undefined;
        backoff = 1_000;
        schedule(0);
      } catch (error) {
        // The session did not receive this batch. Ask lookout to requeue these
        // exact ids on its next invocation, including mixed-task batches.
        pending = undefined;
        requeue = batch.ids.join(",");
        console.error("Posse OpenCode Notice: session delivery failed", error);
        schedule(backoff);
        backoff = Math.min(backoff * 2, maxBackoff);
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
      if (!id || (session && session !== id)) return;
      if (event.type === "session.idle") {
        session = id;
        idle = true;
        flush();
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
