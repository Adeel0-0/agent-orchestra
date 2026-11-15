import { useMemo, useState } from "react";
import { FileDiff, ChevronRight, RefreshCw, X } from "lucide-react";
import { splitDiff } from "./timeline";
import type { DiffResult } from "./types";
export function DiffPane({
  diff,
  refresh,
  close,
}: {
  diff: DiffResult | null;
  refresh: () => void;
  close: () => void;
}) {
  const files = useMemo(() => splitDiff(diff?.diff ?? ""), [diff?.diff]);
  const [chosen, setChosen] = useState("");
  const selected = files.find((file) => file.path === chosen) ?? files[0];
  return (
    <aside className="changes" aria-label="Changes">
      <div className="pane-heading">
        <FileDiff size={16} />
        <h2>Changes</h2>
        <span className="count">
          {files.length + (diff?.untracked.length ?? 0)}
        </span>
        <button
          className="icon-button"
          aria-label="Refresh changes"
          onClick={refresh}
        >
          <RefreshCw size={14} />
        </button>
        <button
          className="icon-button compact-only"
          aria-label="Close changes"
          onClick={close}
        >
          <X size={15} />
        </button>
      </div>
      {files.length === 0 && !diff?.untracked.length ? (
        <div className="changes-empty">
          <FileDiff size={28} />
          <strong>No changes yet</strong>
          <p>
            {diff?.base_commit === null
              ? "This folder has no saved Git base."
              : "Changed files will appear after the agent edits your project."}
          </p>
        </div>
      ) : (
        <>
          <div className="file-list">
            {files.map((file) => (
              <button
                key={file.path}
                className={`file-item ${selected === file ? "selected" : ""}`}
                onClick={() => setChosen(file.path)}
              >
                <ChevronRight size={13} />
                <span>{file.path}</span>
                <span className="add-count">+{file.added}</span>
                <span className="remove-count">−{file.removed}</span>
              </button>
            ))}
            {diff?.untracked.map((path) => (
              <div className="untracked-file" key={path}>
                <FileDiff size={14} />
                <span>{path}</span>
                <small>New file</small>
              </div>
            ))}
          </div>
          {selected && (
            <div className="diff-view">
              <div className="diff-file-title">{selected.path}</div>
              <pre aria-label={`Diff for ${selected.path}`}>
                {selected.lines
                  .filter((line) => !line.startsWith("diff --git "))
                  .map((line, index) => (
                    <div
                      key={index}
                      className={`diff-line ${line.startsWith("+") && !line.startsWith("+++") ? "added" : line.startsWith("-") && !line.startsWith("---") ? "removed" : line.startsWith("@@") ? "hunk" : ""}`}
                    >
                      {line || " "}
                    </div>
                  ))}
              </pre>
            </div>
          )}
        </>
      )}
      {diff?.truncated && (
        <p className="pane-note">
          Large diff truncated. Inspect the remaining changes in your Git
          client.
        </p>
      )}
      <footer className="changes-footer">
        Changes against the session’s starting commit
      </footer>
    </aside>
  );
}
