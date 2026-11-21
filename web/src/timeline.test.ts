import { describe, expect, it } from "vitest";
import { mergeEvents, timeline, splitDiff } from "./timeline";
describe("durable transcript presentation", () => {
  it("deduplicates replay offsets and preserves event order", () => {
    const merged = mergeEvents(
      [{ seq: 1, type: "message.user", text: "hi" }],
      [
        { seq: 1, type: "message.user", text: "hi" },
        { seq: 2, type: "text.delta", text: "hello" },
      ],
    );
    expect(merged.map((e) => e.seq)).toEqual([1, 2]);
  });
  it("assembles streamed tool output and resolves approvals", () => {
    const events = [
      { seq: 1, type: "message.user", text: "edit" },
      { seq: 2, type: "text.delta", text: "I will " },
      { seq: 3, type: "text.delta", text: "inspect." },
      {
        seq: 4,
        type: "tool.started",
        id: "t",
        name: "edit_file",
        input: { path: "a.py" },
      },
      {
        seq: 5,
        type: "permission.requested",
        request_id: "r",
        tool: "edit_file",
        input: { path: "a.py" },
      },
      { seq: 6, type: "permission.answered", request_id: "r", allow: true },
      { seq: 7, type: "tool.output", id: "t", chunk: "first" },
      { seq: 8, type: "tool.output", id: "t", chunk: "second" },
      { seq: 9, type: "tool.finished", id: "t", ok: true },
    ];
    const rows = timeline(events);
    expect(
      rows.find((r) => r.kind === "message" && r.role === "assistant")?.text,
    ).toBe("I will inspect.");
    expect(rows.find((r) => r.kind === "tool")?.output).toBe("firstsecond");
    expect(rows.find((r) => r.kind === "permission")?.answered).toBe(true);
  });
  it("does not leave a cancelled tool looking active", () => {
    const rows = timeline([
      { seq: 1, type: "tool.started", id: "t", name: "run_command", input: {} },
      { seq: 2, type: "turn.completed", stop_reason: "cancelled" },
    ]);
    expect(rows[0].status).toBe("cancelled");
  });
  it("separates file diffs without treating +++ headers as added source", () => {
    const files = splitDiff(
      "diff --git a/a.py b/a.py\n--- a/a.py\n+++ b/a.py\n@@ -1 +1 @@\n-old\n+new\n",
    );
    expect(files[0].path).toBe("a.py");
    expect(files[0].added).toBe(1);
    expect(files[0].removed).toBe(1);
  });
});
