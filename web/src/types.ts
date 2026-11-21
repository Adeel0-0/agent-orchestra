export type Project = {
  id: string;
  path: string;
  name: string;
  last_opened: number;
};
export type Session = {
  id: string;
  project_id: string;
  player: string;
  model: string | null;
  permission: string;
  title: string;
  status: string;
  backend_session_id: string | null;
};
export type Model = { id: string; name?: string; context_length?: number };
export type Health = {
  id: string;
  name: string;
  ready: boolean;
  path: string | null;
  logged_in: boolean | null;
  fix: string | null;
  models: Model[];
  models_error: string | null;
};
export type Settings = {
  default_player: string;
  permission: string;
  openrouter_key_present: boolean;
};
export type DiffResult = {
  base_commit: string | null;
  diff: string;
  untracked: string[];
  truncated: boolean;
};
export type StreamEvent = { seq: number; type: string; [key: string]: unknown };
export type Theme = "system" | "light" | "dark";
