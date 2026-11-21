import { useEffect, useRef, useState } from "react";
import type { Api } from "./api";
import type { StreamEvent, Theme } from "./types";
import { mergeEvents } from "./timeline";

export function useSessionEvents(
  api: Api,
  session: string | null,
  onSettled: () => void,
) {
  const [events, setEvents] = useState<StreamEvent[]>([]),
    [connection, setConnection] = useState("connecting");
  const cache = useRef(new Map<string, StreamEvent[]>()),
    callback = useRef(onSettled);
  callback.current = onSettled;
  useEffect(() => {
    if (!session) {
      setEvents([]);
      setConnection("idle");
      return;
    }
    const sid = session;
    let stopped = false,
      socket: WebSocket | undefined,
      timer: ReturnType<typeof setTimeout> | undefined,
      attempt = 0,
      pending: StreamEvent[] = [];
    setEvents(cache.current.get(sid) ?? []);
    function flush() {
      if (!pending.length) return;
      const next = mergeEvents(cache.current.get(sid) ?? [], pending);
      pending = [];
      cache.current.set(sid, next);
      if (!stopped) setEvents(next);
    }
    function open() {
      if (stopped) return;
      setConnection(attempt ? "reconnecting" : "connecting");
      const last = cache.current.get(sid)?.at(-1)?.seq ?? 0;
      socket = api.socket(sid, last);
      socket.onopen = () => {
        attempt = 0;
        if (!stopped) setConnection("connected");
      };
      socket.onmessage = (message) => {
        try {
          const event = JSON.parse(message.data) as StreamEvent;
          if (Number.isInteger(event.seq) && typeof event.type === "string") {
            pending.push(event);
            if (event.type === "turn.completed" || event.type === "file.changed") {
              flush();
              callback.current();
            }
          }
        } catch {
          /* malformed data never becomes HTML */
        }
      };
      socket.onclose = () => {
        flush();
        if (!stopped) {
          setConnection("reconnecting");
          timer = setTimeout(open, Math.min(5000, 500 * 2 ** attempt++));
        }
      };
      socket.onerror = () => socket?.close();
    }
    open();
    const interval = setInterval(flush, 50);
    return () => {
      flush();
      stopped = true;
      clearInterval(interval);
      clearTimeout(timer);
      socket?.close();
      if (cache.current.size > 20)
        cache.current.delete(cache.current.keys().next().value!);
    };
  }, [api, session]);
  return { events, connection };
}

export function useTheme() {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      return (localStorage.getItem("orchestra-theme") as Theme) || "system";
    } catch {
      return "system";
    }
  });
  useEffect(() => {
    const query = matchMedia("(prefers-color-scheme: dark)");
    const apply = () => {
      document.documentElement.dataset.theme =
        theme === "system" ? (query.matches ? "dark" : "light") : theme;
    };
    apply();
    query.addEventListener("change", apply);
    try {
      localStorage.setItem("orchestra-theme", theme);
    } catch {
      /* appearance only */
    }
    return () => query.removeEventListener("change", apply);
  }, [theme]);
  return { theme, setTheme };
}
