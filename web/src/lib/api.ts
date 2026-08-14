// Shared HTTP plumbing for the management console (admin-console). Every call
// carries the bearer token from localStorage, the same credential the chat
// endpoint uses, and surfaces the server's `error` field as a thrown Error so
// callers can render one message rather than decoding status codes.

import { clearToken, getToken } from "@/lib/auth";

export class ApiError extends Error {
  // Declared as a field rather than a constructor parameter property: the
  // project builds with erasableSyntaxOnly, which forbids the shorthand.
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

// handleUnauthorized signs the session out when the server answers 401 on an
// AUTHENTICATED request: the stored token is no longer valid (expired,
// revoked, or replaced), so it is cleared and the app returns to the login
// screen (App listens for the auth:expired event). Callers whose 401 is an
// EXPECTED outcome — the login and phone routes, which never attach a token —
// must not call it; without a stored token this is a no-op.
export function handleUnauthorized(res: { status: number }): void {
  if (res.status !== 401 || !getToken()) return;
  clearToken();
  window.dispatchEvent(new Event("auth:expired"));
}

type Options = {
  method?: string;
  body?: unknown;
  // contentType overrides the automatic application/json (for raw/binary
  // payloads like the image upload endpoint, which reads the request body
  // directly rather than parsing JSON).
  contentType?: string;
  // raw returns the response body as a Blob instead of JSON-parsing it (the
  // data-export download, say).
  raw?: boolean;
  // signal lets a component abandon an in-flight request when it unmounts or
  // when a newer request supersedes it.
  signal?: AbortSignal;
};

// apiTimeoutMs bounds requests that carry no caller signal. Every server
// endpoint has its own deadline, but a half-open TCP connection (server
// process gone without a FIN) would otherwise hang fetch forever and the
// console would spin indefinitely. 60s keeps uploads and exports safe;
// callers that need finer control pass their own signal, which wins outright.
const apiTimeoutMs = 60_000;

export async function api<T>(path: string, opts: Options = {}): Promise<T> {
  const token = getToken();
  const headers: Record<string, string> = {};
  if (token) headers.authorization = `Bearer ${token}`;
  if (opts.body !== undefined) {
    headers["content-type"] = opts.contentType ?? "application/json";
  }

  // A caller-provided signal wins outright (no watchdog); otherwise a timeout
  // aborts the request after apiTimeoutMs so a dead connection surfaces as an
  // ApiError instead of an endless spinner.
  const controller = new AbortController();
  const signal = opts.signal ?? controller.signal;
  const timer = setTimeout(() => controller.abort(), apiTimeoutMs);
  let res: Response;
  try {
    res = await fetch(path, {
      method: opts.method ?? "GET",
      headers,
      // JSON callers pass an object to stringify; a contentType override signals a
      // raw binary body (the image upload endpoint) passed through untouched.
      body:
        opts.body === undefined
          ? undefined
          : opts.contentType
            ? (opts.body as BodyInit)
            : JSON.stringify(opts.body),
      signal,
    });
  } catch (err) {
    if (opts.signal) throw err; // caller-owned abort propagates untouched
    // No caller signal, so this abort is the watchdog's: the request never
    // settled. Report it as a request-timeout error, not the raw AbortError.
    throw new ApiError("request timed out", 408);
  } finally {
    clearTimeout(timer);
  }

  // An expired/revoked token answers 401 on every protected route: sign out
  // rather than surfacing each request's generic error to a dead session.
  handleUnauthorized(res);

  if (res.status === 204) return undefined as T;
  if (opts.raw) {
    if (!res.ok) {
      const text = await res.text();
      throw new ApiError(
        (() => {
          try {
            return (JSON.parse(text) as { error?: string }).error ?? `request failed (${res.status})`;
          } catch {
            return text.slice(0, 200) || `request failed (${res.status})`;
          }
        })(),
        res.status,
      );
    }
    return (await res.blob()) as T;
  }

  const text = await res.text();
  let data: unknown = undefined;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      // A non-JSON body from an error page is still worth reporting verbatim.
      if (!res.ok) throw new ApiError(text.slice(0, 200), res.status);
    }
  }

  if (!res.ok) {
    const msg =
      (data as { error?: string } | undefined)?.error ??
      `request failed (${res.status})`;
    throw new ApiError(msg, res.status);
  }
  return data as T;
}

// qs builds a query string from defined values only, so an unset filter is
// absent rather than sent as the string "undefined".
export function qs(params: Record<string, string | number | undefined>): string {
  const parts = Object.entries(params)
    .filter(([, v]) => v !== undefined && v !== "")
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  return parts.length > 0 ? `?${parts.join("&")}` : "";
}

// uploadUserImage stores an image as a user-level upload (change
// user-image-uploads), independent of any session — so a brand-new
// conversation's first message can carry an image. The returned path has the
// "uploads/<id>.webp" form the message wire format resolves.
export async function uploadUserImage(file: File): Promise<{ path: string }> {
  return api<{ path: string }>("/api/chat/uploads", {
    method: "POST",
    body: await file.arrayBuffer(),
    contentType: file.type || "application/octet-stream",
  });
}
