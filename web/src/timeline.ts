import type { StreamEvent } from "./types";
export type Entry = {
  key: string;
  kind:
    | "message"
    | "reasoning"
    | "tool"
    | "permission"
    | "notice"
    | "error"
    | "usage";
  role?: "user" | "assistant";
  text?: string;
  id?: string;
  name?: string;
  input?: unknown;
  output?: string;
  status?: string;
  answered?: boolean;
  allow?: boolean;
  data?: StreamEvent;
};
export function mergeEvents(
  current: StreamEvent[],
  incoming: StreamEvent[],
): StreamEvent[] {
  const map = new Map(current.map((event) => [event.seq, event]));
  for (const event of incoming)
    if (Number.isInteger(event.seq) && !map.has(event.seq))
      map.set(event.seq, event);
  return [...map.values()].sort((a, b) => a.seq - b.seq);
}
export function timeline(events: StreamEvent[]): Entry[] {
  const rows: Entry[] = [],
    tools = new Map<string, Entry>(),
    approvals = new Map<string, Entry>();
  for (const e of events) {
    const key = String(e.seq),
      last = rows.at(-1);
    if (e.type === "message.user")
      rows.push({
        key,
        kind: "message",
        role: "user",
        text: String(e.text ?? ""),
      });
    else if (e.type === "text.delta" || e.type === "reasoning.delta") {
      const kind = e.type === "text.delta" ? "message" : "reasoning";
      if (
        last?.kind === kind &&
        (kind === "reasoning" || last.role === "assistant")
      )
        last.text += String(e.text ?? "");
      else
        rows.push({ key, kind, role: "assistant", text: String(e.text ?? "") });
    } else if (e.type === "tool.started") {
      const row: Entry = {
        key,
        kind: "tool",
        id: String(e.id),
        name: String(e.name),
        input: e.input,
        output: "",
        status: "running",
      };
      tools.set(String(e.id), row);
      rows.push(row);
    } else if (e.type === "tool.output") {
      const row = tools.get(String(e.id));
      if (row) row.output += String(e.chunk ?? "");
    } else if (e.type === "tool.finished") {
      const row = tools.get(String(e.id));
      if (row) row.status = e.ok ? "done" : "failed";
    } else if (e.type === "permission.requested") {
      const row: Entry = {
        key,
        kind: "permission",
        id: String(e.request_id),
        name: String(e.tool),
        input: e.input,
        text: String(e.reason ?? ""),
        answered: false,
      };
      approvals.set(String(e.request_id), row);
      rows.push(row);
    } else if (e.type === "permission.answered") {
      const row = approvals.get(String(e.request_id));
      if (row) {
        row.answered = true;
        row.allow = Boolean(e.allow);
      }
    } else if (e.type === "turn.completed") {
      for (const row of tools.values())
        if (row.status === "running")
          row.status =
            String(e.stop_reason) === "end_turn"
              ? "done"
              : String(e.stop_reason);
      for (const row of approvals.values())
        if (!row.answered) {
          row.answered = true;
          row.text = "Request ended with this turn.";
        }
    } else if (e.type === "error" || e.type === "notice")
      rows.push({ key, kind: e.type, text: String(e.message ?? "") });
    else if (e.type === "usage") rows.push({ key, kind: "usage", data: e });
  }
  return rows;
}
export type FileDiff = {
  path: string;
  lines: string[];
  added: number;
  removed: number;
};
export function splitDiff(diff: string): FileDiff[] {
  const files: FileDiff[] = [];
  let file: FileDiff | undefined;
  for (const line of diff.split("\n")) {
    if (line.startsWith("diff --git ")) {
      const path = line.match(/ b\/(.+)$/)?.[1] ?? line;
      file = { path, lines: [], added: 0, removed: 0 };
      files.push(file);
    }
    if (file) {
      file.lines.push(line);
      if (line.startsWith("+") && !line.startsWith("+++")) file.added++;
      if (line.startsWith("-") && !line.startsWith("---")) file.removed++;
    }
  }
  return files;
}
export function activity(events: StreamEvent[], fallback: string): string {
  let state = fallback;
  for (const e of events) {
    if (e.type === "message.user" || e.type === "permission.answered")
      state = "running";
    if (e.type === "permission.requested") state = "waiting_permission";
    if (e.type === "turn.completed")
      state = e.stop_reason === "end_turn" ? "idle" : String(e.stop_reason);
  }
  return state;
}
