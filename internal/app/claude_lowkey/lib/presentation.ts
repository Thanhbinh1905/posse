// Presentation-only policy. No prompt, tool or stored message is changed.
// The zero-height, restored-row and redraw approach is adapted from firstmate-calm
// (MIT, https://github.com/kunchenguid/firstmate).
export function lowkeyValue(text: string | undefined): boolean | undefined {
  if (text === undefined) return undefined;
  const section = text.match(/(?:^|\n)\s*\[lowkey\]\s*\n([\s\S]*?)(?=\n\s*\[|$)/);
  const lead = section?.[1]?.match(/^\s*lead\s*=\s*(true|false)\s*(?:#.*)?$/m);
  return lead ? lead[1] === "true" : undefined;
}

// Modify only the lowkey.lead key; preserve every other project setting and comment.
export function setLowkey(text: string, active: boolean): string {
  const section = /(^|\n)(\s*\[lowkey\]\s*\n)([\s\S]*?)(?=\n\s*\[|$)/m;
  if (!section.test(text)) return `${text.replace(/\s*$/, "\n")}[lowkey]\nlead = ${active}\n`;
  return text.replace(section, (_whole, before: string, header: string, body: string) => {
    const line = /^(\s*lead\s*=\s*)(true|false)(\s*(?:#.*)?)$/m;
    return before + header + (line.test(body)
      ? body.replace(line, (_match, prefix: string, _value: string, suffix: string) => prefix + active + suffix)
      : `lead = ${active}\n` + body);
  });
}

// Accept only the complete, well-formed Posse -> Lead Notice envelope. Text in a
// JSON-quoted body never supplies the routing header. A prefix alone is not a Notice.
export function isPosseNotice(text: string): boolean {
  const match = /^\[posse \| Posse -> Lead (?:project|t[1-9]\d*(?:,t[1-9]\d*)*) \| notice #([1-9]\d*(?:,[1-9]\d*)*)\]\nbody: ("(?:[^"\\\x00-\x1f]|\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))*")$/.exec(text);
  if (!match) return false;
  try { return typeof JSON.parse(match[2]!) === "string"; } catch { return false; }
}

export function noticeRecordName(text: string): string | undefined {
  if (!isPosseNotice(text)) return undefined;
  const ids = text.match(/^\[posse \| Posse -> Lead [^\n]+ \| notice #([1-9]\d*(?:,[1-9]\d*)*)\]/)?.[1];
  return ids === undefined ? undefined : ids + ".txt";
}

export function textKey(text: string): string { return text.trim(); }
export type SessionRow = { role: string; text: string; toolUses: readonly unknown[] };
// Restored rows include tool calls either on the text row or in the next assistant
// row. Stop at the next user turn. Never apply a 240-character exemption.
export function restoredRows(rows: readonly SessionRow[]): { notes: Set<string>; replies: Set<string> } {
  const notes = new Set<string>();
  const replies = new Set<string>();
  for (let i = 0; i < rows.length; i++) {
    const row = rows[i]!;
    if (row.role !== "assistant" || !textKey(row.text)) continue;
    let tools = row.toolUses?.length > 0;
    for (let j = i + 1; !tools && j < rows.length && rows[j]!.role === "assistant"; j++) {
      tools = rows[j]!.toolUses?.length > 0;
    }
    (tools ? notes : replies).add(textKey(row.text));
  }
  for (const reply of replies) notes.delete(reply);
  return { notes, replies };
}
