export class Api {
  constructor(private readonly token: string) {}
  async request<T>(
    path: string,
    method = "GET",
    data?: unknown,
    signal?: AbortSignal,
  ): Promise<T> {
    const response = await fetch(path, {
      method,
      signal,
      headers: {
        Authorization: `Bearer ${this.token}`,
        ...(data === undefined ? {} : { "Content-Type": "application/json" }),
      },
      body: data === undefined ? undefined : JSON.stringify(data),
    });
    if (!response.ok) {
      const error = await response
        .json()
        .catch(() => ({ error: `Request failed (${response.status})` }));
      throw new Error(String(error.error));
    }
    return response.json() as Promise<T>;
  }
  socket(session: string, after: number): WebSocket {
    return new WebSocket(
      `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/sessions/${encodeURIComponent(session)}/events?after=${after}`,
      ["orchestra", `orchestra.token.${this.token}`],
    );
  }
}
export async function connect(): Promise<Api> {
  const nonce = new URLSearchParams(location.hash.slice(1)).get("bootstrap");
  history.replaceState(null, "", location.pathname + location.search);
  if (!nonce)
    throw new Error(
      "This window needs a fresh connection. Reopen Orchestra from the launcher.",
    );
  const response = await fetch("/api/bootstrap", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ nonce }),
  });
  const data = await response.json();
  if (!response.ok)
    throw new Error(String(data.error ?? "Reopen Orchestra to connect."));
  return new Api(String(data.token));
}
