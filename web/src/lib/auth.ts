// Minimal token-based auth against the nowhere-agent identity endpoints.
// The token is stored in localStorage and sent as a Bearer header on /api/chat.

const KEY = "nowhere.token";

export function getToken(): string | null {
  return localStorage.getItem(KEY);
}

export function clearToken(): void {
  localStorage.removeItem(KEY);
}

async function post(path: string, body: unknown): Promise<Response> {
  return fetch(path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

async function auth(
  path: string,
  body: { email: string; password: string; display_name?: string },
): Promise<void> {
  const res = await post(path, body);
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
  const data = (await res.json()) as { token?: string };
  // signup returns only the user; login returns the token. For signup we
  // immediately log in to obtain a token.
  if (data.token) {
    localStorage.setItem(KEY, data.token);
    return;
  }
  const login = await post("/api/auth/login", {
    email: body.email,
    password: body.password,
  });
  if (!login.ok) throw new Error("login after signup failed");
  const lj = (await login.json()) as { token: string };
  localStorage.setItem(KEY, lj.token);
}

export function login(email: string, password: string): Promise<void> {
  return auth("/api/auth/login", { email, password });
}

export function signup(email: string, password: string): Promise<void> {
  return auth("/api/auth/signup", { email, password, display_name: email });
}

export async function logout(): Promise<void> {
  const token = getToken();
  if (token) {
    await fetch("/api/auth/logout", {
      method: "POST",
      headers: { authorization: `Bearer ${token}` },
    }).catch(() => {});
  }
  clearToken();
}
