import { useEffect, useMemo, useRef, useState } from "react";
import Markdown from "react-markdown";
import {
  Check,
  ChevronRight,
  Terminal,
  FilePenLine,
  ShieldCheck,
  AlertTriangle,
  ArrowDown,
  LoaderCircle,
} from "lucide-react";
import type { Api } from "./api";
import type { StreamEvent } from "./types";
import { timeline, type Entry } from "./timeline";
const labels: Record<string, string> = {
  read_file: "Read file",
  write_file: "Write file",
  edit_file: "Edit file",
  run_command: "Run command",
  list_dir: "List folder",
  grep: "Search text",
  glob: "Find files",
};
function content(input: unknown) {
  return JSON.stringify(input, null, 2);
}
function Approval({
  row,
  api,
  sid,
  onError,
}: {
  row: Entry;
  api: Api;
  sid: string;
  onError: (message: string) => void;
}) {
  const [pending, setPending] = useState(false),
    [answered, setAnswered] = useState<boolean | null>(null);
  async function answer(allow: boolean) {
    setPending(true);
    try {
      await api.request(
        `/api/sessions/${sid}/permissions/${encodeURIComponent(row.id!)}`,
        "POST",
        { allow },
      );
      setAnswered(allow);
    } catch (error) {
      onError(String(error));
    } finally {
      setPending(false);
    }
  }
  const done = row.answered || answered !== null;
  return (
    <section
      className={`approval ${done ? "answered" : ""}`}
      aria-label="Action approval"
    >
      <div className="approval-heading">
        <ShieldCheck size={18} />
        <strong>
          {done ? "Permission recorded" : "Your approval is needed"}
        </strong>
      </div>
      <p>
        {done
          ? (row.allow ?? answered)
            ? "Allowed"
            : "Denied or ended"
          : row.text}
      </p>
      <pre>{content(row.input)}</pre>
      {!done && (
        <div className="approval-actions">
          <button
            className="button primary"
            disabled={pending}
            onClick={() => answer(true)}
          >
            {row.name === "run_command" ? "Allow command" : "Allow edit"}
          </button>
          <button
            className="button"
            disabled={pending}
            onClick={() => answer(false)}
          >
            Deny
          </button>
          <span>Only this action</span>
        </div>
      )}
    </section>
  );
}
function Tool({ row }: { row: Entry }) {
  const input = row.input as Record<string, unknown> | undefined;
  const summary = String(input?.path ?? input?.command ?? input?.pattern ?? "");
  return (
    <details className={`tool-card ${row.status}`}>
      <summary>
        <span className="tool-icon">
          {row.name === "run_command" ? (
            <Terminal size={15} />
          ) : (
            <FilePenLine size={15} />
          )}
        </span>
        <span className="tool-name">{labels[row.name!] ?? row.name}</span>
        <span className="tool-summary">{summary}</span>
        <span className="tool-status">
          {row.status === "running" ? (
            <LoaderCircle size={13} className="spin" />
          ) : row.status === "done" ? (
            <Check size={13} />
          ) : (
            <AlertTriangle size={13} />
          )}{" "}
          {row.status === "done"
            ? "Done"
            : row.status === "running"
              ? "Running"
              : row.status}
        </span>
        <ChevronRight size={14} className="disclosure" />
      </summary>
      <div className="tool-body">
        <span className="field-caption">Input</span>
        <pre>{content(row.input)}</pre>
        {row.output && (
          <>
            <span className="field-caption">Output</span>
            <pre>{row.output}</pre>
          </>
        )}
      </div>
    </details>
  );
}
export function Transcript({
  events,
  api,
  sid,
  onError,
  active,
}: {
  events: StreamEvent[];
  api: Api;
  sid: string;
  onError: (s: string) => void;
  active: boolean;
}) {
  const rows = useMemo(() => timeline(events), [events]);
  const scroller = useRef<HTMLDivElement>(null),
    near = useRef(true);
  const [showJump, setShowJump] = useState(false);
  useEffect(() => {
    near.current = true;
    setShowJump(false);
  }, [sid]);
  useEffect(() => {
    if (near.current && scroller.current)
      scroller.current.scrollTop = scroller.current.scrollHeight;
    else setShowJump(true);
  }, [events]);
  return (
    <div className="transcript-wrap">
      <div
        className="transcript"
        ref={scroller}
        onScroll={() => {
          const element = scroller.current!;
          near.current =
            element.scrollHeight - element.scrollTop - element.clientHeight <
            120;
          if (near.current) setShowJump(false);
        }}
      >
        <div className="transcript-content">
          {rows.length === 0 && (
            <div className="conversation-empty">
              <span className="conversation-mark">◎</span>
              <h2>What are we working on?</h2>
              <p>
                Ask your agent to explore the project, make a change, or check
                its work.
              </p>
              <p className="quiet">
                Actions and changes will appear here as they happen.
              </p>
            </div>
          )}
          {rows.map((row) =>
            row.kind === "message" ? (
              <article key={row.key} className={`message ${row.role}`}>
                <div className="message-author">
                  {row.role === "user" ? "You" : "Orchestra"}
                </div>
                <div className="message-copy">
                  <Markdown
                    components={{
                      a: ({ href, children }) => (
                        <a href={href} target="_blank" rel="noreferrer">
                          {children}
                        </a>
                      ),
                    }}
                  >
                    {row.text ?? ""}
                  </Markdown>
                </div>
              </article>
            ) : row.kind === "tool" ? (
              <Tool key={row.key} row={row} />
            ) : row.kind === "permission" ? (
              <Approval
                key={`${sid}-${row.key}`}
                row={row}
                api={api}
                sid={sid}
                onError={onError}
              />
            ) : row.kind === "reasoning" ? (
              <details key={row.key} className="reasoning">
                <summary>Reasoning</summary>
                <Markdown>{row.text ?? ""}</Markdown>
              </details>
            ) : row.kind === "usage" ? (
              <div key={row.key} className="usage">
                {Number(row.data?.input_tokens ?? 0).toLocaleString()} input ·{" "}
                {Number(row.data?.output_tokens ?? 0).toLocaleString()} output
                tokens
                {row.data?.cost_usd != null
                  ? ` · $${Number(row.data.cost_usd).toFixed(5)}`
                  : ""}
              </div>
            ) : (
              <div
                key={row.key}
                className={`transcript-notice ${row.kind}`}
                role={row.kind === "error" ? "alert" : undefined}
              >
                <AlertTriangle size={15} />
                <span>{row.text}</span>
              </div>
            ),
          )}
          {active && (
            <div className="thinking" role="status">
              <span className="thinking-dots" />
              Working on this turn…
            </div>
          )}
        </div>
      </div>
      {showJump && (
        <button
          className="jump-latest button"
          onClick={() => {
            near.current = true;
            scroller.current?.scrollTo({ top: scroller.current.scrollHeight });
            setShowJump(false);
          }}
        >
          <ArrowDown size={14} />
          Latest messages
        </button>
      )}
    </div>
  );
}
