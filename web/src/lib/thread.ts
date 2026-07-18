// Session (thread) persistence: the backend owns the session id and streams it
// to us in a data-session frame; we keep it so multi-turn chat reuses one
// session and a page reload can replay its history.

const KEY = "nowhere.thread";

export function getSessionId(): string | null {
  return localStorage.getItem(KEY);
}

export function setSessionId(id: string): void {
  localStorage.setItem(KEY, id);
}

export function clearSessionId(): void {
  localStorage.removeItem(KEY);
}
