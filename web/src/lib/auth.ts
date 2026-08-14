// Minimal token-based auth against the nowhere-agent identity endpoints.
// The token lives in sessionStorage — a per-tab store that dies with the
// browser window, so a stolen token's exposure window is a single session
// instead of weeks on disk. Tokens left in localStorage by older builds are
// migrated on first read (one-shot) and dropped from their old home.

const KEY = "nowhere.token";

export function getToken(): string | null {
  const token = sessionStorage.getItem(KEY);
  if (token !== null) return token;
  // One-shot migration from localStorage (pre-sessionStorage deployments):
  // take the legacy token and drop it from its old home.
  const legacy = localStorage.getItem(KEY);
  if (legacy !== null) {
    sessionStorage.setItem(KEY, legacy);
    localStorage.removeItem(KEY);
  }
  return legacy;
}

export function setToken(token: string): void {
  sessionStorage.setItem(KEY, token);
}

export function clearToken(): void {
  sessionStorage.removeItem(KEY);
  localStorage.removeItem(KEY);
}

// consumeSSORedirect reads the OIDC callback's URL-fragment hand-off
// (/#token=... on success, /#totp_required=<challenge> when the account has a
// second factor, /#sso_error=... on failure), stores a token if one arrived,
// and strips the fragment from the address bar so the credential does not
// linger in history or get copied into a shared link. The server delivers
// the token in a fragment precisely so it never reaches a server log; we honor
// that by removing it here. Returns the outcome so the caller can render an
// SSO error or proceed signed-in.
export function consumeSSORedirect(): {
  token: string | null;
  error: string | null;
  totpRequired: string | null;
} {
  const hash = window.location.hash;
  if (!hash) return { token: null, error: null, totpRequired: null };
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  const token = params.get("token");
  const error = params.get("sso_error");
  const totpRequired = params.get("totp_required");
  if (!token && !error && !totpRequired)
    return { token: null, error: null, totpRequired: null };
  if (token) setToken(token);
  // Strip the fragment (token, challenge, or error) without adding a history
  // entry.
  window.history.replaceState(null, "", window.location.pathname + window.location.search);
  return { token, error, totpRequired };
}


async function post(path: string, body: unknown): Promise<Response> {
  return fetch(path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

// loginResult carries the two possible login outcomes: a bearer token, or a
// second-factor challenge (totp_required) the caller must complete.
export type LoginResult =
  | { token: string; totp_required?: false }
  | { totp_required: true; totp_token: string };

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
    setToken(data.token);
    return;
  }
  const login = await post("/api/auth/login", {
    email: body.email,
    password: body.password,
  });
  if (!login.ok) throw new Error("login after signup failed");
  const lj = (await login.json()) as { token: string };
  setToken(lj.token);
}

// login verifies credentials. On success it stores the bearer token; when the
// account has a second factor it returns the challenge instead of a token.
export async function login(
  email: string,
  password: string,
): Promise<LoginResult> {
  const res = await post("/api/auth/login", { email, password });
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
  const data = (await res.json()) as {
    token?: string;
    totp_required?: boolean;
    totp_token?: string;
  };
  if (data.token) {
    setToken(data.token);
    return { token: data.token };
  }
  if (data.totp_required && data.totp_token) {
    return { totp_required: true, totp_token: data.totp_token };
  }
  throw new Error("login failed: no token returned");
}

// completeTotpLogin redeems the challenge token with the authenticator code
// and stores the resulting bearer token.
export async function completeTotpLogin(
  totpToken: string,
  code: string,
): Promise<void> {
  const res = await post("/api/auth/totp/verify", {
    totp_token: totpToken,
    code,
  });
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
  const data = (await res.json()) as { token?: string };
  if (!data.token) throw new Error("verification failed: no token returned");
  setToken(data.token);
}

export function signup(email: string, password: string): Promise<void> {
  return auth("/api/auth/signup", { email, password, display_name: email });
}

// requestPhoneCode asks the server to deliver a verification code to phone
// (the deployment's SMS gateway does the actual sending).
export async function requestPhoneCode(phone: string): Promise<void> {
  const res = await post("/api/auth/phone/request-code", { phone });
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
}

// verifyPhoneCode checks the code; on success the server provisions/resolves
// the account and returns the platform token, stored like any other login.
export async function verifyPhoneCode(phone: string, code: string): Promise<void> {
  const res = await post("/api/auth/phone/verify", { phone, code });
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
  const data = (await res.json()) as { token?: string };
  if (!data.token) throw new Error("no token returned");
  setToken(data.token);
}

// phoneAuthAvailable probes the server for the phone/OTP routes (404 when not
// configured), mirroring the OIDC probe.
export async function phoneAuthAvailable(): Promise<boolean> {
  try {
    const res = await fetch("/api/auth/phone/enabled");
    return res.ok;
  } catch {
    return false;
  }
}

// resetPasswordWithPhone verifies the code delivered to a BOUND phone and sets
// a new password for its account — the self-service recovery path shown from
// the login page's "forgot password" link. The server revokes every session.
export async function resetPasswordWithPhone(
  phone: string,
  code: string,
  password: string,
): Promise<void> {
  const res = await post("/api/auth/phone/reset-password", { phone, code, password });
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
}

// requestEmailResetCode asks the server to mint a one-time reset code for the
// account's email. There is no mail channel yet: the code is printed to the
// server log (dev/self-host path), like the phone channel's log:// mode.
export async function requestEmailResetCode(email: string): Promise<void> {
  const res = await post("/api/auth/email/reset-code", { email });
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
}

// resetPasswordWithEmail verifies the reset code addressed to the account's
// email and sets a new password. The server revokes every session.
export async function resetPasswordWithEmail(
  email: string,
  code: string,
  password: string,
): Promise<void> {
  const res = await post("/api/auth/email/reset-password", { email, code, password });
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(data.error ?? `request failed (${res.status})`);
  }
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
