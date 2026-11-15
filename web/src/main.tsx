import { createRoot } from "react-dom/client";
import { App } from "./App";
import { connect } from "./api";
import "./styles.css";
const root = createRoot(document.getElementById("root")!);
root.render(
  <div className="boot-screen" role="status">
    Opening Orchestra…
  </div>,
);
connect()
  .then((api) => root.render(<App api={api} />))
  .catch((error) =>
    root.render(
      <main className="boot-screen boot-error">
        <span className="boot-mark">◎</span>
        <h1>Reconnect to your workspace</h1>
        <p>
          {error instanceof Error
            ? error.message
            : "Reopen Orchestra from the launcher."}
        </p>
        <code>orchestra ui</code>
        <small>
          No project data is available until this window reconnects.
        </small>
      </main>,
    ),
  );
