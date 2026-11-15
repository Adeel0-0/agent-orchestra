import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import {
  AudioLines,
  FolderOpen,
  Plus,
  Settings as SettingsIcon,
  PanelLeft,
  PanelRight,
  Send,
  Square,
  X,
  MessageSquare,
  Check,
  ArrowUpRight,
  ShieldCheck,
  RefreshCw,
  Search,
  ChevronDown,
} from "lucide-react";
import type { Api } from "./api";
import type { Project, Session, Health, Settings, DiffResult } from "./types";
import { useSessionEvents, useTheme } from "./hooks";
import { activity } from "./timeline";
import { Transcript } from "./Transcript";
import { DiffPane } from "./DiffPane";

const AGENTS = [
  { id: "codex", name: "Codex" },
  { id: "claude", name: "Claude Code" },
  { id: "antigravity", name: "Antigravity" },
  { id: "openrouter", name: "OpenRouter" },
];
const agentName = (id: string) =>
  AGENTS.find((agent) => agent.id === id)?.name ?? id;
const statusName = (status: string) =>
  ({
    idle: "Ready",
    running: "Working",
    waiting_permission: "Approval needed",
    cancelled: "Stopped",
    interrupted: "Interrupted",
    cleanup_failed: "Needs attention",
    error: "Error",
    cancelling: "Stopping",
  })[status] ?? status;
const message = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
function Dialog({
  title,
  children,
  close,
  label,
  error,
}: {
  title: string;
  children: ReactNode;
  close: () => void;
  label?: string;
  error?: string;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const element = dialog.current!;
    element.showModal();
    return () => element.close();
  }, []);
  return (
    <dialog
      ref={dialog}
      className="dialog"
      onCancel={(event) => {
        event.preventDefault();
        close();
      }}
    >
      <div className="dialog-heading">
        <h2>{title}</h2>
        <button
          className="icon-button"
          aria-label={label ?? `Close ${title.toLowerCase()}`}
          onClick={close}
        >
          <X size={18} />
        </button>
      </div>
      {error && (
        <p className="global-error" role="alert">
          {error}
        </p>
      )}
      {children}
    </dialog>
  );
}

export function App({ api }: { api: Api }) {
  const [projects, setProjects] = useState<Project[]>([]),
    [projectId, setProjectId] = useState<string | null>(null),
    [sessions, setSessions] = useState<Session[]>([]),
    [sessionId, setSessionId] = useState<string | null>(null);
  const [health, setHealth] = useState<Health[]>([]),
    [healthLoading, setHealthLoading] = useState(true),
    [loading, setLoading] = useState(true),
    [nativePicker, setNativePicker] = useState(false),
    [error, setError] = useState("");
  const [settings, setSettings] = useState<Settings>({
    default_player: "codex",
    permission: "edit",
    openrouter_key_present: false,
  });
  const [agent, setAgent] = useState("codex"),
    [model, setModel] = useState(""),
    [prompt, setPrompt] = useState(""),
    [starting, setStarting] = useState(false),
    [submitting, setSubmitting] = useState(false);
  const [dialog, setDialog] = useState<
      "folder" | "session" | "settings" | "projects" | null
    >(null),
    [folder, setFolder] = useState(""),
    [sessionAgent, setSessionAgent] = useState("codex"),
    [sessionModel, setSessionModel] = useState(""),
    [permission, setPermission] = useState("edit"),
    [modalBusy, setModalBusy] = useState(false);
  const [diff, setDiff] = useState<DiffResult | null>(null),
    [showDiff, setShowDiff] = useState(innerWidth >= 1150),
    [showNav, setShowNav] = useState(innerWidth >= 760),
    [filter, setFilter] = useState("");
  const composer = useRef<HTMLTextAreaElement>(null),
    projectSearch = useRef<HTMLInputElement>(null),
    agentSelect = useRef<HTMLSelectElement>(null),
    submittedSeq = useRef(0);
  const { theme, setTheme } = useTheme();
  // Each selection has its own request generation, including A -> B -> A.
  // A slow response from an earlier selection must never replace this view.
  const projectScope = useRef({ key: projectId, request: 0 });
  const viewKey = JSON.stringify([projectId, sessionId]);
  const viewScope = useRef({ key: viewKey, request: 0 });
  if (projectScope.current.key !== projectId)
    projectScope.current = { key: projectId, request: 0 };
  if (viewScope.current.key !== viewKey)
    viewScope.current = { key: viewKey, request: 0 };
  const project = projects.find((item) => item.id === projectId),
    session = sessions.find((item) => item.id === sessionId);
  const allModels =
    health.find((item) => item.id === "openrouter")?.models ?? [];

  useEffect(() => {
    const narrow = matchMedia("(max-width: 1149px)");
    const small = matchMedia("(max-width: 759px)");
    const collapseDiff = () => {
      if (narrow.matches) setShowDiff(false);
    };
    const collapseNav = () => {
      if (small.matches) setShowNav(false);
    };
    narrow.addEventListener("change", collapseDiff);
    small.addEventListener("change", collapseNav);
    return () => {
      narrow.removeEventListener("change", collapseDiff);
      small.removeEventListener("change", collapseNav);
    };
  }, []);

  const refreshHealth = useCallback(async () => {
    setHealthLoading(true);
    try {
      setHealth(await api.request<Health[]>("/api/players"));
    } catch (e) {
      setError(message(e));
    } finally {
      setHealthLoading(false);
    }
  }, [api]);
  useEffect(() => {
    let live = true;
    Promise.all([
      api.request<Project[]>("/api/projects"),
      api.request<Settings>("/api/settings"),
      api.request<{ native_folder_picker: boolean }>("/api/capabilities"),
    ])
      .then(([list, preferences, capabilities]) => {
        if (!live) return;
        setProjects(list);
        setSettings(preferences);
        setAgent(preferences.default_player);
        setSessionAgent(preferences.default_player);
        setPermission(preferences.permission);
        setNativePicker(capabilities.native_folder_picker);
        if (list.length) setProjectId(list[0].id);
      })
      .catch((e) => live && setError(message(e)))
      .finally(() => live && setLoading(false));
    void refreshHealth();
    return () => {
      live = false;
    };
  }, [api, refreshHealth]);
  useEffect(() => {
    setSessionId(null);
    setSessions([]);
    setDiff(null);
    if (!projectId) return;
    const abort = new AbortController();
    const scope = projectScope.current;
    const request = ++scope.request;
    api
      .request<Session[]>(
        `/api/projects/${projectId}/sessions`,
        "GET",
        undefined,
        abort.signal,
      )
      .then((list) => {
        if (
          abort.signal.aborted ||
          projectScope.current !== scope ||
          scope.request !== request
        )
          return;
        setSessions(list);
        if (list.length) setSessionId(list[0].id);
      })
      .catch((e) => {
        if (!abort.signal.aborted) setError(message(e));
      });
    return () => abort.abort();
  }, [api, projectId]);
  useEffect(() => {
    if (session) {
      setAgent(session.player);
      setModel(session.model ?? "");
    }
    setStarting(false);
  }, [sessionId]);
  const refreshSessions = useCallback(async () => {
    if (projectId) {
      const scope = projectScope.current;
      if (scope.key !== projectId) return;
      const request = ++scope.request;
      try {
        const list = await api.request<Session[]>(
          `/api/projects/${projectId}/sessions`,
        );
        if (projectScope.current === scope && scope.request === request)
          setSessions(list);
      } catch (e) {
        if (projectScope.current === scope && scope.request === request)
          setError(message(e));
      }
    }
  }, [api, projectId]);
  const refreshDiff = useCallback(async () => {
    if (sessionId) {
      const scope = viewScope.current;
      if (scope.key !== JSON.stringify([projectId, sessionId])) return;
      const request = ++scope.request;
      try {
        const result = await api.request<DiffResult>(
          `/api/sessions/${sessionId}/diff`,
        );
        if (viewScope.current === scope && scope.request === request)
          setDiff(result);
      } catch (e) {
        if (viewScope.current === scope && scope.request === request)
          setError(message(e));
      }
    }
  }, [api, projectId, sessionId]);
  useEffect(() => {
    setDiff(null);
    void refreshDiff();
  }, [refreshDiff]);
  const settled = () => {
    void refreshSessions().catch((e) => setError(message(e)));
    void refreshDiff();
  };
  const { events, connection } = useSessionEvents(api, sessionId, settled);
  useEffect(() => {
    if ((events.at(-1)?.seq ?? 0) > submittedSeq.current) setStarting(false);
  }, [events]);
  const currentStatus = activity(events, session?.status ?? "idle"),
    busy =
      starting ||
      ["running", "waiting_permission", "cancelling"].includes(currentStatus);
  const changedBackend = Boolean(
    session &&
      (agent !== session.player || model.trim() !== (session.model ?? "")),
  );

  async function addProject(path: string) {
    if (!path.trim()) return;
    setModalBusy(true);
    try {
      const added = await api.request<Project>("/api/projects", "POST", {
        path: path.trim(),
      });
      setProjects(await api.request<Project[]>("/api/projects"));
      setProjectId(added.id);
      setDialog(null);
      setFolder("");
      setShowNav(innerWidth >= 760);
    } catch (e) {
      setError(message(e));
    } finally {
      setModalBusy(false);
    }
  }
  async function openFolder() {
    setError("");
    if (nativePicker) {
      setModalBusy(true);
      try {
        const result = await api.request<{ path: string | null }>(
          "/api/folders/pick",
          "POST",
          {},
        );
        if (result.path) await addProject(result.path);
      } catch (e) {
        setError(message(e));
      } finally {
        setModalBusy(false);
      }
    } else setDialog("folder");
  }
  function newSession() {
    if (!projectId) {
      void openFolder();
      return;
    }
    setSessionAgent(agent || settings.default_player);
    setSessionModel(model);
    setPermission(settings.permission);
    setDialog("session");
  }
  async function createSession(
    chosenAgent: string,
    chosenModel: string,
    chosenPermission: string,
  ): Promise<Session> {
    if (!projectId) throw new Error("Open a project first.");
    if (chosenAgent === "openrouter" && !chosenModel.trim())
      throw new Error("Choose an OpenRouter model.");
    const scope = viewScope.current;
    const created = await api.request<Session>(
      `/api/projects/${projectId}/sessions`,
      "POST",
      {
        player: chosenAgent,
        model: chosenModel.trim() || null,
        permission: chosenPermission,
      },
    );
    if (viewScope.current !== scope)
      throw new Error(
        "Selection changed. The empty session was saved in its original project; select it there to continue.",
      );
    // Invalidate a list request started before this session was created.
    ++projectScope.current.request;
    setSessions((list) => [created, ...list]);
    setSessionId(created.id);
    setAgent(created.player);
    setModel(created.model ?? "");
    return created;
  }
  async function submit(event?: FormEvent) {
    event?.preventDefault();
    if (!prompt.trim() || busy || submitting) return;
    setError("");
    setSubmitting(true);
    let active = session;
    const scope = projectScope.current;
    try {
      if (!active || changedBackend)
        active = await createSession(
          agent,
          model,
          session?.permission ?? settings.permission,
        );
      if (projectScope.current !== scope) return;
      submittedSeq.current =
        active.id === sessionId ? (events.at(-1)?.seq ?? 0) : 0;
      setStarting(true);
      await api.request(`/api/sessions/${active.id}/messages`, "POST", {
        prompt,
      });
      if (
        viewScope.current.key === JSON.stringify([active.project_id, active.id])
      )
        setPrompt("");
      await refreshSessions();
      composer.current?.focus();
    } catch (e) {
      setError(message(e));
      setStarting(false);
    } finally {
      setSubmitting(false);
    }
  }
  async function stop() {
    if (!sessionId) return;
    try {
      await api.request(`/api/sessions/${sessionId}/cancel`, "POST");
      setStarting(false);
      settled();
    } catch (e) {
      setError(message(e));
    }
  }
  async function recover() {
    if (
      !sessionId ||
      !confirm(
        "Confirm that you inspected and stopped any leftover agent or command processes. This confirmation does not stop them for you.",
      )
    )
      return;
    try {
      await api.request(`/api/sessions/${sessionId}/cancel`, "POST", {
        confirm_stopped: true,
      });
      settled();
    } catch (e) {
      setError(message(e));
    }
  }
  useEffect(() => {
    function shortcut(event: KeyboardEvent) {
      if (dialog) return;
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "n") {
        event.preventDefault();
        newSession();
      }
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setDialog("projects");
      }
      if ((event.ctrlKey || event.metaKey) && event.key === ",") {
        event.preventDefault();
        setDialog("settings");
      }
      if (
        (event.ctrlKey || event.metaKey) &&
        event.shiftKey &&
        event.key.toLowerCase() === "p"
      ) {
        event.preventDefault();
        agentSelect.current?.focus();
      }
      if (event.key === "Escape" && busy) {
        event.preventDefault();
        void stop();
      }
    }
    window.addEventListener("keydown", shortcut);
    return () => window.removeEventListener("keydown", shortcut);
  });
  const available = health.filter((item) => item.ready).length;
  return (
    <div className="workspace" data-nav={showNav} data-diff={showDiff}>
      <aside className="sidebar" aria-label="Projects and sessions">
        <div className="brand">
          <span className="brand-mark">
            <AudioLines size={21} />
          </span>
          <strong>Orchestra</strong>
          <span className="local-label">Local</span>
        </div>
        <button
          className="button open-folder"
          onClick={() => void openFolder()}
          disabled={modalBusy}
        >
          <FolderOpen size={16} />
          Open folder<kbd>⌘</kbd>
        </button>
        <div className="rail-title">
          <h2>Projects</h2>
          <button
            className="icon-button"
            aria-label="Find project"
            title="Find project (Ctrl+K)"
            onClick={() => setDialog("projects")}
          >
            <Search size={14} />
          </button>
        </div>
        <nav className="project-list">
          {projects.length === 0 ? (
            <p className="rail-empty">Your projects stay on this machine.</p>
          ) : (
            projects.map((item) => (
              <div className="project-group" key={item.id}>
                <button
                  className={`project-item ${item.id === projectId ? "selected" : ""}`}
                  onClick={() => {
                    setProjectId(item.id);
                    setShowNav(innerWidth >= 760);
                  }}
                  title={item.path}
                >
                  <FolderOpen size={15} />
                  <span>{item.name}</span>
                  <ChevronDown size={13} />
                </button>
                {item.id === projectId && (
                  <div className="session-list">
                    {sessions.map((item) => (
                      <button
                        key={item.id}
                        className={`session-item ${item.id === sessionId ? "selected" : ""}`}
                        onClick={() => {
                          setSessionId(item.id);
                          setShowNav(innerWidth >= 760);
                        }}
                        title={`${item.title} — ${agentName(item.player)}`}
                      >
                        <span className={`status-dot ${item.status}`} />
                        <span>{item.title}</span>
                      </button>
                    ))}
                    <button className="new-session-inline" onClick={newSession}>
                      <Plus size={14} />
                      New session
                    </button>
                  </div>
                )}
              </div>
            ))
          )}
        </nav>
        <div className="rail-bottom">
          <div className="agent-summary">
            <span className={`status-dot ${available ? "ready" : "unknown"}`} />
            {healthLoading
              ? "Checking agents…"
              : `${available} agent${available === 1 ? "" : "s"} ready`}
            <button
              className="icon-button"
              aria-label="Refresh agents"
              onClick={() => void refreshHealth()}
            >
              <RefreshCw size={13} />
            </button>
          </div>
          <button
            className="settings-link"
            aria-label="Settings"
            onClick={() => setDialog("settings")}
          >
            <SettingsIcon size={16} />
            Settings<span>Ctrl ,</span>
          </button>
        </div>
      </aside>
      <main className="conversation">
        <header className="workspace-header">
          <button
            className="icon-button"
            aria-label="Toggle projects"
            onClick={() => setShowNav((value) => !value)}
          >
            <PanelLeft size={17} />
          </button>
          <div className="header-context">
            <span>{project?.name ?? "Your workspace"}</span>
            <ChevronDown size={12} />
            <strong>{session?.title ?? "New conversation"}</strong>
          </div>
          <div className="header-right">
            {session && (
              <span className={`session-state ${currentStatus}`}>
                <span className={`status-dot ${currentStatus}`} />
                {statusName(currentStatus)}
              </span>
            )}
            <button
              className="icon-button"
              aria-label="New session"
              title="New session (Ctrl+N)"
              onClick={newSession}
            >
              <Plus size={18} />
            </button>
            <button
              className={`icon-button ${showDiff ? "pressed" : ""}`}
              aria-label="Toggle changes"
              onClick={() => setShowDiff((value) => !value)}
            >
              <PanelRight size={17} />
            </button>
          </div>
        </header>
        {error && (
          <div className="global-error" role="alert">
            <span>{error}</span>
            <button
              className="icon-button"
              aria-label="Dismiss error"
              onClick={() => setError("")}
            >
              <X size={14} />
            </button>
          </div>
        )}
        {loading ? (
          <div className="loading-screen" role="status">
            Opening your workspace…
          </div>
        ) : !project ? (
          <section className="welcome">
            <div className="welcome-symbol">
              <AudioLines size={36} />
            </div>
            <h1>Open a project to start</h1>
            <p>
              A place for your code, your agents,
              <br />
              and the next thing you want to build.
            </p>
            <button
              className="button primary"
              onClick={() => void openFolder()}
              disabled={modalBusy}
            >
              <FolderOpen size={17} />
              Open folder
            </button>
            <div className="agent-health">
              <div className="section-heading">
                <h2>Your agents</h2>
                <span>
                  {healthLoading ? "Checking availability…" : "On this machine"}
                </span>
              </div>
              {health.map((item) => (
                <div className="health-row" key={item.id}>
                  <span className={`agent-avatar ${item.id}`}>
                    {agentName(item.id)[0]}
                  </span>
                  <div>
                    <strong>{agentName(item.id)}</strong>
                    <span>
                      {item.ready
                        ? "Ready to work"
                        : item.path
                          ? "Sign in or configure access"
                          : "Not set up"}
                    </span>
                    {!item.ready && item.fix && <code>{item.fix}</code>}
                  </div>
                  {item.ready ? (
                    <Check size={16} />
                  ) : (
                    <ArrowUpRight size={15} />
                  )}
                </div>
              ))}
            </div>
            <p className="welcome-footer">
              <ShieldCheck size={13} />
              Your projects and conversations stay on your machine.
            </p>
          </section>
        ) : (
          <>
            <div className="project-location" title={project.path}>
              <FolderOpen size={12} />
              {project.path}
              <span>
                {session ? agentName(session.player) : "Choose an agent below"}
              </span>
            </div>
            {sessionId ? (
              <Transcript
                key={sessionId}
                events={events}
                api={api}
                sid={sessionId}
                active={busy && currentStatus !== "waiting_permission"}
                onError={setError}
              />
            ) : (
              <div className="no-session">
                <MessageSquare size={30} />
                <h2>Start a conversation</h2>
                <p>
                  Ask a question below, or create a session with your preferred
                  agent.
                </p>
                <button className="button" onClick={newSession}>
                  <Plus size={15} />
                  New session
                </button>
              </div>
            )}
            {["interrupted", "cleanup_failed"].includes(
              session?.status ?? "",
            ) && (
              <div className="recovery-banner">
                <strong>This session needs recovery.</strong>
                <span>
                  Inspect and stop leftover processes before continuing.
                </span>
                <button className="button" onClick={() => void recover()}>
                  Confirm processes stopped
                </button>
              </div>
            )}
            <div className="composer-area">
              <form className="composer" onSubmit={submit}>
                <textarea
                  ref={composer}
                  aria-label="Message"
                  placeholder={
                    busy
                      ? "A turn is in progress…"
                      : `Ask ${agentName(agent)} to work on ${project.name}…`
                  }
                  value={prompt}
                  disabled={busy}
                  onChange={(event) => setPrompt(event.target.value)}
                  onKeyDown={(event) => {
                    if (
                      event.key === "Enter" &&
                      !event.shiftKey &&
                      !event.nativeEvent.isComposing
                    ) {
                      event.preventDefault();
                      void submit();
                    }
                  }}
                  rows={3}
                />
                <div className="composer-controls">
                  <label className="agent-picker">
                    <span className={`mini-avatar ${agent}`} />
                    <select
                      aria-label="Agent"
                      ref={agentSelect}
                      value={agent}
                      disabled={busy}
                      onChange={(event) => {
                        setAgent(event.target.value);
                        setModel("");
                      }}
                    >
                      {AGENTS.map((item) => (
                        <option key={item.id} value={item.id}>
                          {item.name}
                        </option>
                      ))}
                    </select>
                  </label>
                  <label className="model-picker">
                    <input
                      aria-label="Model"
                      list="composer-models"
                      placeholder="Default model"
                      value={model}
                      disabled={busy}
                      onChange={(event) => setModel(event.target.value)}
                    />
                    <datalist id="composer-models">
                      {allModels.map((item) => (
                        <option key={item.id} value={item.id}>
                          {item.name ?? item.id}
                        </option>
                      ))}
                    </datalist>
                  </label>
                  <span className="permission-label">
                    <ShieldCheck size={12} />
                    {session?.permission === "read-only"
                      ? "Read-only / ask"
                      : session?.permission === "full"
                        ? "Full access"
                        : "Edit mode"}
                  </span>
                  {busy ? (
                    <button
                      type="button"
                      className="stop-button"
                      aria-label="Stop turn"
                      onClick={() => void stop()}
                    >
                      <Square size={13} fill="currentColor" />
                      Stop
                    </button>
                  ) : (
                    <button
                      className="send-button"
                      aria-label="Send message"
                      disabled={!prompt.trim() || submitting}
                    >
                      <Send size={17} />
                    </button>
                  )}
                </div>
              </form>
              <div className="composer-footnote">
                <span>
                  {changedBackend
                    ? "A new session will start with this agent and model."
                    : connection === "reconnecting"
                      ? "Reconnecting. Your agent continues working."
                      : "Enter to send · Shift+Enter for a new line"}
                </span>
                <span>Local workspace</span>
              </div>
            </div>
          </>
        )}
      </main>
      <DiffPane
        diff={diff}
        refresh={() => void refreshDiff()}
        close={() => setShowDiff(false)}
      />
      {dialog === "folder" && (
        <Dialog
          title="Open a project"
          error={error}
          close={() => setDialog(null)}
        >
          <p className="dialog-copy">
            Enter the full path to a folder on this machine. Your files stay in
            place.
          </p>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              void addProject(folder);
            }}
          >
            <label className="field">
              Folder path
              <input
                autoFocus
                value={folder}
                onChange={(event) => setFolder(event.target.value)}
                placeholder="D:\\projects\\my-project"
                required
              />
            </label>
            <div className="dialog-actions">
              <button
                type="button"
                className="button"
                onClick={() => setDialog(null)}
              >
                Cancel
              </button>
              <button
                className="button primary"
                disabled={modalBusy || !folder.trim()}
              >
                Open project
              </button>
            </div>
          </form>
        </Dialog>
      )}
      {dialog === "session" && (
        <Dialog title="New session" error={error} close={() => setDialog(null)}>
          <p className="dialog-copy">
            A fresh conversation in <strong>{project?.name}</strong>.
          </p>
          <form
            onSubmit={async (event) => {
              event.preventDefault();
              setModalBusy(true);
              try {
                await createSession(sessionAgent, sessionModel, permission);
                setDialog(null);
                composer.current?.focus();
              } catch (e) {
                setError(message(e));
              } finally {
                setModalBusy(false);
              }
            }}
          >
            <label className="field">
              Agent
              <select
                aria-label="Session agent"
                value={sessionAgent}
                onChange={(event) => {
                  setSessionAgent(event.target.value);
                  setSessionModel("");
                }}
              >
                {AGENTS.map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.name}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              Model
              <input
                aria-label="Session model"
                list="session-models"
                value={sessionModel}
                onChange={(event) => setSessionModel(event.target.value)}
                placeholder={
                  sessionAgent === "openrouter"
                    ? "Choose a model"
                    : "Use agent default"
                }
              />
              <datalist id="session-models">
                {allModels.map((item) => (
                  <option key={item.id} value={item.id} />
                ))}
              </datalist>
            </label>
            <label className="field">
              Permissions
              <select
                aria-label="Session permissions"
                value={permission}
                onChange={(event) => setPermission(event.target.value)}
              >
                <option value="read-only">
                  Read-only / ask before changes
                </option>
                <option value="edit">Edit project files</option>
                <option value="full">Full access</option>
              </select>
            </label>
            <p className="field-help">
              CLI agents use their own sandbox. OpenRouter asks for each command
              in edit mode. Full access allows commands without a prompt.
            </p>
            <div className="dialog-actions">
              <button
                type="button"
                className="button"
                onClick={() => setDialog(null)}
              >
                Cancel
              </button>
              <button className="button primary" disabled={modalBusy}>
                Create session
              </button>
            </div>
          </form>
        </Dialog>
      )}
      {dialog === "settings" && (
        <Dialog
          title="Settings"
          error={error}
          label="Close settings"
          close={() => setDialog(null)}
        >
          <label className="field">
            Appearance
            <select
              aria-label="Appearance"
              value={theme}
              onChange={(event) => setTheme(event.target.value as typeof theme)}
            >
              <option value="system">Follow system</option>
              <option value="light">Light</option>
              <option value="dark">Dark</option>
            </select>
          </label>
          <label className="field">
            Default agent
            <select
              value={settings.default_player}
              onChange={async (event) => {
                try {
                  setSettings(
                    await api.request<Settings>("/api/settings", "PUT", {
                      default_player: event.target.value,
                    }),
                  );
                } catch (e) {
                  setError(message(e));
                }
              }}
            >
              {AGENTS.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.name}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            Default permissions
            <select
              value={settings.permission}
              onChange={async (event) => {
                try {
                  setSettings(
                    await api.request<Settings>("/api/settings", "PUT", {
                      permission: event.target.value,
                    }),
                  );
                } catch (e) {
                  setError(message(e));
                }
              }}
            >
              <option value="read-only">Read-only / ask</option>
              <option value="edit">Edit project files</option>
              <option value="full">Full access</option>
            </select>
          </label>
          <div className="settings-section">
            <h3>OpenRouter access</h3>
            <p>
              {settings.openrouter_key_present
                ? "A key is saved in your OS credential manager."
                : "No key is configured. Enter it privately in a terminal:"}
            </p>
            {!settings.openrouter_key_present && (
              <code>python -m orchestra openrouter-key</code>
            )}
            <p className="field-help">
              API credits are separate from agent subscriptions. Keys are never
              entered in this window.
            </p>
          </div>
          <div className="settings-shortcuts">
            <span>
              New session<kbd>Ctrl N</kbd>
            </span>
            <span>
              Switch project<kbd>Ctrl K</kbd>
            </span>
            <span>
              Choose agent<kbd>Ctrl Shift P</kbd>
            </span>
            <span>
              Stop turn<kbd>Esc</kbd>
            </span>
          </div>
        </Dialog>
      )}
      {dialog === "projects" && (
        <Dialog title="Switch project" close={() => setDialog(null)}>
          <label className="field">
            Find a project
            <input
              ref={projectSearch}
              autoFocus
              value={filter}
              onChange={(event) => setFilter(event.target.value)}
              placeholder="Search by name or path"
            />
          </label>
          <div className="project-search-results">
            {projects
              .filter((item) =>
                `${item.name} ${item.path}`
                  .toLowerCase()
                  .includes(filter.toLowerCase()),
              )
              .map((item) => (
                <button
                  key={item.id}
                  onClick={() => {
                    setProjectId(item.id);
                    setDialog(null);
                    setFilter("");
                  }}
                >
                  <FolderOpen size={17} />
                  <span>
                    <strong>{item.name}</strong>
                    <small>{item.path}</small>
                  </span>
                </button>
              ))}
          </div>
        </Dialog>
      )}
    </div>
  );
}
