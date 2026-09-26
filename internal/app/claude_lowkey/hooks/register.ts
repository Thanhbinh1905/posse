// Posse Claude Lead lowkey mod. Presentation-only, adapted from firstmate-calm
// (MIT, https://github.com/kunchenguid/firstmate). The function-hook API is
// early access. Missing capabilities leave the stock renderer in charge.
import type { EngineInterface, Register, RenderInput, RenderElement } from "claude-code";
import { noticeRecordName, lowkeyValue, restoredRows, setLowkey, textKey } from "../lib/presentation.ts";

const frames = ["░░▒▓", "░▒▓█", "▒▓█▓", "▓█▓▒", "█▓▒░", "▓▒░░"];
const rasterKey = "posse-lowkey-slab";
let enabled = false;
let supported = false;
let loaded: Promise<void> | undefined;
let path = "";
let globalPath = "";
let noticesDir = "";
let notes = new Set<string>();
let replies = new Set<string>();
let frame = 0;
let sites = new Map<string, number>();
let timer: { cancel(): void } | undefined;

function hidden($: EngineInterface, e: RenderInput): RenderElement {
  return $.ui.resolve(e).Box({ display: "none" });
}

// Raster encoding and zero-height Box technique adapted from firstmate-calm.
function slab(columns: number): string {
  const width = Math.max(1, Math.min(512, columns));
  const cells = new Uint32Array(width * 3);
  const label = frames[frame]!;
  for (let i = 0; i < width; i++) {
    cells[i * 3] = (label[i] ?? " ").codePointAt(0)!;
    cells[i * 3 + 1] = i < label.length ? 0x93a5ff : 0x01000000;
    cells[i * 3 + 2] = 0x01000000;
  }
  const bytes = new Uint8Array(cells.buffer);
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  let result = "";
  for (let i = 0; i < bytes.length; i += 3) {
    const word = (bytes[i]! << 16) | ((bytes[i + 1] ?? 0) << 8) | (bytes[i + 2] ?? 0);
    result += alphabet[(word >> 18) & 63] + alphabet[(word >> 12) & 63] +
      (i + 1 < bytes.length ? alphabet[(word >> 6) & 63] : "=") +
      (i + 2 < bytes.length ? alphabet[word & 63] : "=");
  }
  return result;
}

async function refresh($: EngineInterface): Promise<void> {
  let local: string | undefined;
  let global: string | undefined;
  try { local = await $.fs.read(path); } catch { /* project override absent */ }
  const localValue = lowkeyValue(local);
  if (localValue === undefined) {
    try { global = await $.fs.read(globalPath); } catch { /* default off */ }
  }
  const active = localValue ?? lowkeyValue(global) ?? false;
  if (enabled !== active) {
    enabled = active;
    if (!active) sites.clear();
    $.ui.invalidate("ui.render");
  }
}

async function load($: EngineInterface): Promise<void> {
  // Calls below probe the seams. If a capability is removed, load fails closed.
  supported = false;
  path = await $.env.get("POSSE_LOWKEY_CONFIG") ?? "";
  globalPath = await $.env.get("POSSE_LOWKEY_GLOBAL_CONFIG") ?? "";
  noticesDir = await $.env.get("POSSE_LOWKEY_NOTICES_DIR") ?? "";
  if (!path || !globalPath || !noticesDir) { supported = false; return; }
  try {
    const restored = restoredRows(await $.session.messages());
    notes = restored.notes;
    replies = restored.replies;
    await refresh($);
    timer ??= $.clock.every(225, () => { void tick($); });
    supported = true;
  } catch {
    supported = false;
    enabled = false;
  }
}

async function ready($: EngineInterface): Promise<boolean> {
  if ((await $.env.get("CLAUDE_CODE_ENABLE_FUNCTION_HOOKS")) !== "1") return false;
  loaded ??= load($);
  await loaded;
  return supported;
}

async function tick($: EngineInterface): Promise<void> {
  if (!enabled || sites.size === 0) return;
  frame = (frame + 1) % frames.length;
  for (const [requestId, columns] of sites) {
    try {
      const result = await $.ui.blit({ requestId, key: rasterKey, columns, rows: 1, cells: slab(columns) });
      if (result.deny !== undefined) sites.delete(requestId);
    } catch {
      // The animation seam is gone: stop filtering and return to stock drawings.
      sites.clear(); enabled = false; supported = false; timer?.cancel(); timer = undefined;
      try { $.ui.invalidate("ui.render"); } catch { /* unsupported renderer */ }
      return;
    }
  }
}

export const register: Register = (on) => {
  on("session.start", async ($, e, next) => {
    timer?.cancel(); timer = undefined;
    loaded = undefined; notes.clear(); replies.clear(); sites.clear(); enabled = false;
    if (await ready($)) {
      try { await $.command.register({ name: "lowkey", description: "Toggle the Lead's lowkey presentation." }); }
      catch { supported = false; enabled = false; }
    }
    return next(e);
  });

  on("command.run", { command: "lowkey" }, async ($, e, next) => {
    if (!(await ready($))) return next(e);
    const action = e.args.trim() || "toggle";
    if (!["toggle", "on", "off", "status"].includes(action)) {
      $.ui.toast("Usage: /lowkey [on|off|status]"); return {};
    }
    await refresh($);
    if (action === "status") { $.ui.toast(`Lowkey ${enabled ? "on" : "off"}`); return {}; }
    const active = action === "toggle" ? !enabled : action === "on";
    try {
      let content = "";
      try { content = await $.fs.read(path); } catch { /* create project override */ }
      await $.fs.write(path, setLowkey(content, active));
      enabled = active;
      if (!enabled) sites.clear();
      $.ui.invalidate("ui.render");
      $.ui.toast(active ? "Lowkey on" : "Lowkey off");
    } catch (error) { $.ui.toast(`Lowkey unchanged: ${error}`); }
    return {};
  });

  on("turn.step", async function* ($, e, next) {
    if (!(await ready($))) {
      const stream = next(e);
      for await (const chunk of stream) yield chunk;
      return await stream.result;
    }
    const stream = next(e);
    const blocks = new Map<number, string>();
    for await (const chunk of stream) {
      if (chunk.kind === "text") blocks.set(chunk.index, (blocks.get(chunk.index) ?? "") + chunk.text);
      yield chunk;
    }
    const result = await stream.result;
    if (e.agentId === undefined) {
      const usesTools = result.stopReason === "tool_use" ||
        (result.stopReason === "max_tokens" && result.toolUses.length > 0);
      let changed = false;
      for (const text of [...blocks.values(), result.answer]) {
        const key = textKey(text);
        if (!key) continue;
        if (usesTools) { if (!notes.has(key)) { notes.add(key); changed = true; } }
        else { if (!replies.has(key)) { replies.add(key); changed = true; } }
      }
      if (changed && enabled) $.ui.invalidate("ui.render");
    }
    return result;
  });

  for (const component of ["ToolUse", "ToolResult", "ToolGroup"] as const) {
    on("ui.render", { component }, async ($, e, next) => {
      if (!(await ready($))) return next(e);
      try { await refresh($); return enabled ? hidden($, e) : next(e); }
      catch { supported = false; enabled = false; return next(e); }
    });
  }
  on("ui.render", { component: "UserMessage" }, async ($, e, next) => {
    if (!(await ready($))) return next(e);
    try {
      await refresh($);
      if (!enabled) return next(e);
      const record = noticeRecordName(e.props.text);
      if (record === undefined) return next(e);
      let attested: string | undefined;
      try { attested = await $.fs.read(`${noticesDir}/${record}`); } catch { /* User text, not a recorded Notice */ }
      return attested === e.props.text ? hidden($, e) : next(e);
    } catch { supported = false; enabled = false; return next(e); }
  });
  on("ui.render", { component: "AssistantMessage" }, async ($, e, next) => {
    if (!(await ready($))) return next(e);
    try {
      await refresh($);
      const key = textKey(e.props.text);
      return enabled && notes.has(key) && !replies.has(key) ? hidden($, e) : next(e);
    } catch { supported = false; enabled = false; return next(e); }
  });
  on("ui.render", { component: "Spinner" }, async ($, e, next) => {
    if (!(await ready($))) return next(e);
    try {
      await refresh($);
      if (!enabled || e.surface !== "terminal") { sites.delete(e.requestId); return next(e); }
      const columns = Math.max(1, Math.min(512, (e.viewport?.columns ?? 80) - 2));
      sites.set(e.requestId, columns);
      const { Box, Raster } = $.ui.resolve(e);
      return Box({ children: Raster({ key: rasterKey, columns, rows: 1, cells: slab(columns) }) });
    } catch {
      sites.delete(e.requestId); supported = false; enabled = false;
      return next(e);
    }
  });
};
