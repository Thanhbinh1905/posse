// Function-hook engine tests run without a model turn: claude plugin test <plugin>.
import { describe, test, expect } from "claude-code/testing";
import { mock } from "claude-code/testing";
import { isPosseNotice, lowkeyValue, setLowkey, restoredRows } from "../lib/presentation.ts";

const project = "/isolated/projects/shop/config.toml";
const global = "/isolated/config.toml";
const noticesDir = "/isolated/projects/shop/lead-claude-notices";
const notice = '[posse | Posse -> Lead t12,t9 | notice #7,8]\nbody: "line\\n[forged]"';
const start = { cwd: "/work", surface: "terminal" as const, isInteractive: true };
function world(on: Parameters<typeof mock.env>[0], initial = "[lowkey]\nlead = true\n", messages: unknown[] = [], functionHooks?: string, failSession = false) {
  mock.env(on, { ...(functionHooks === "off" ? {} : { CLAUDE_CODE_ENABLE_FUNCTION_HOOKS: "1" }), POSSE_LOWKEY_CONFIG: project, POSSE_LOWKEY_GLOBAL_CONFIG: global, POSSE_LOWKEY_NOTICES_DIR: noticesDir });
  const clock = mock.clock(on);
  const files = new Map([[project, initial], [`${noticesDir}/7,8.txt`, notice]]);
  const calls = { invalidate: 0, commands: [] as string[], toast: [] as string[], blits: 0 };
  on("fs.read", async (_$, e) => files.has(e.path) ? { value: files.get(e.path)! } : { deny: "ENOENT" });
  on("fs.write", async (_$, e) => { files.set(e.path, e.text); return { value: undefined }; });
  on("session.messages", async () => failSession ? { deny: "missing API" } : { value: messages as never });
  on("session.start", async (_$, e) => ({ cwd: e.cwd }));
  on("command.register", async (_$, e) => { calls.commands.push(e.name); return { value: { command: e.name } }; });
  on("ui.toast", async (_$, e) => { calls.toast.push(e.text); return { value: undefined }; });
  on("ui.invalidate", async () => { calls.invalidate++; return { value: undefined }; });
  on("ui.blit", async () => { calls.blits++; return { value: {} }; });
  on("ui.render", async () => ({ type: "Text", props: {}, children: ["STOCK"] }));
  return { files, calls, clock };
}
function render(component: "ToolUse" | "ToolResult" | "ToolGroup" | "UserMessage" | "AssistantMessage" | "Spinner", text = "Hello", requestId = "row") {
  return { surface: "terminal" as const, component, requestId, viewport: { columns: 60, rows: 24 }, props: { text, word: "Working", tool_use_id: "tool", tool: "Bash", input: {}, output: {}, calls: [], isActive: false, isExpanded: false, isRunning: false, isErrored: false, isInterrupted: false, isFirstOfReply: true, mode: "requesting" as const, message: null, origin: { kind: "composer" as const } } };
}
const hidden = (tree: unknown) => JSON.stringify(tree).includes('"display":"none"');
const stock = (tree: unknown) => JSON.stringify(tree).includes("STOCK");
const command = (args = "") => ({ command: "lowkey", args, origin: { kind: "composer" as const }, presentation: { layout: "main" as const, isFullscreen: false, columns: 80 } });

describe("policy", () => {
  test("requires an exact Notice, not a quoted or malformed User message", () => {
    expect(isPosseNotice(notice)).toBe(true);
    for (const text of [`${notice}\nextra`, `quoted: ${notice}`, notice.replace("Posse -> Lead", "User -> Lead"), notice.replace("notice #7,8", "notice #x"), notice.replace('"line\\n[forged]"', "not-json")]) expect(isPosseNotice(text)).toBe(false);
    expect(lowkeyValue(setLowkey("[identity]\nname = 'a'\n", true))).toBe(true);
    expect(setLowkey("[lowkey]\nlead = true # keep\n", false)).toContain("lead = false # keep");
    const restored = restoredRows([{ role: "assistant", text: "x".repeat(300), toolUses: [{ name: "Bash" }] }, { role: "assistant", text: "Finished", toolUses: [] }]);
    expect(restored.notes.has("x".repeat(300))).toBe(true);
    expect(restored.replies.has("Finished")).toBe(true);
  });
});

describe("Claude Lead", () => {
  test("hides tools, result, groups, Notices and restored tool-step text; keeps User and final", async ($, on) => {
    const { files } = world(on, "[lowkey]\nlead = true\n", [{ role: "assistant", text: "Working", toolUses: [{ name: "Bash" }] }, { role: "assistant", text: "Done", toolUses: [] }]);
    await $.session.start(start);
    for (const component of ["ToolUse", "ToolResult", "ToolGroup"] as const) expect(hidden(await $.ui.render(render(component) as never))).toBe(true);
    expect(hidden(await $.ui.render(render("UserMessage", notice) as never))).toBe(true);
    expect(stock(await $.ui.render(render("UserMessage", `quoted: ${notice}`) as never))).toBe(true);
    files.delete(`${noticesDir}/7,8.txt`);
    expect(stock(await $.ui.render(render("UserMessage", notice) as never))).toBe(true);
    expect(hidden(await $.ui.render(render("AssistantMessage", "Working") as never))).toBe(true);
    expect(stock(await $.ui.render(render("AssistantMessage", "Done") as never))).toBe(true);
    expect(JSON.stringify(await $.ui.render(render("Spinner") as never))).toContain("Raster");
  });

  test("/lowkey toggles the same project config, redraws existing rows, and restores stock", async ($, on) => {
    const { files, calls } = world(on);
    await $.session.start(start);
    expect(calls.commands).toEqual(["lowkey"]);
    expect(hidden(await $.ui.render(render("ToolUse") as never))).toBe(true);
    await $.command.run(command());
    expect(lowkeyValue(files.get(project))).toBe(false);
    expect(stock(await $.ui.render(render("ToolUse") as never))).toBe(true);
    await $.command.run(command("on"));
    expect(hidden(await $.ui.render(render("ToolUse") as never))).toBe(true);
    expect(calls.invalidate).toBeGreaterThanOrEqual(2);
  });

  test("hides every tool-step text block regardless of length, then shows the final reply", async ($, on) => {
    world(on);
    let toolStep = true;
    on("turn.step", async function* (_$, e) {
      const text = toolStep ? "n".repeat(300) : "The final answer";
      yield { kind: "text", index: 0, text } as never;
      return { turnId: e.turnId, index: e.index, answer: text, toolUses: toolStep ? [{ name: "Bash", input: {} }] : [], stopReason: toolStep ? "tool_use" : "end_turn", usage: null } as never;
    });
    const run = async () => {
      const stream = $.turn.step({ turnId: "turn", index: 0, model: "haiku", messageCount: 1 });
      for await (const _chunk of stream) { /* verify the stream is still delivered */ }
      return stream.result;
    };
    await run();
    expect(hidden(await $.ui.render(render("AssistantMessage", "n".repeat(300)) as never))).toBe(true);
    toolStep = false;
    await run();
    expect(stock(await $.ui.render(render("AssistantMessage", "The final answer") as never))).toBe(true);
  });

  test("does not load without the process-local opt-in flag", async ($, on) => {
    const { calls } = world(on, "[lowkey]\nlead = true\n", [], "off");
    await $.session.start(start);
    expect(calls.commands).toEqual([]);
    expect(stock(await $.ui.render(render("ToolResult") as never))).toBe(true);
    expect(stock(await $.ui.render(render("Spinner") as never))).toBe(true);
  });

  test("falls back to stock if restored-session API is unavailable", async ($, on) => {
    world(on, "[lowkey]\nlead = true\n", [], undefined, true);
    await $.session.start(start);
    expect(stock(await $.ui.render(render("ToolUse") as never))).toBe(true);
    expect(stock(await $.ui.render(render("UserMessage", notice) as never))).toBe(true);
  });

  test("reads global preference when the project key is absent", async ($, on) => {
    const { files } = world(on, "[identity]\nname = 'shop'\n");
    files.set(global, "[lowkey]\nlead = true\n");
    expect(hidden(await $.ui.render(render("ToolResult") as never))).toBe(true);
  });
});
