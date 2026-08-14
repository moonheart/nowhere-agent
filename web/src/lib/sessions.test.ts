// sessions API unit tests: the paging guard lives in SessionList, but
// listSessions' contract — token-gated fetch, null on ANY failure (network or
// non-OK), parsed page shape, empty cursor on exhaustion — is pure logic worth
// pinning without a browser.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { deleteSession, listSessions } from "@/lib/sessions";

// auth.getToken reads sessionStorage (with a localStorage fallback migration),
// so both storages must exist in the DOM-free node test environment.
function memStorage(seed: Record<string, string> = {}): Storage {
  const m = new Map(Object.entries(seed));
  return {
    getItem: (k) => m.get(k) ?? null,
    setItem: (k, v) => void m.set(k, v),
    removeItem: (k) => void m.delete(k),
  } as Storage;
}

function tokenStore(token: string | null) {
  vi.stubGlobal("localStorage", memStorage());
  vi.stubGlobal(
    "sessionStorage",
    memStorage(token === null ? {} : { "nowhere.token": token }),
  );
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

describe("listSessions", () => {
  it("returns an empty page without a token (no fetch)", async () => {
    tokenStore(null);
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const page = await listSessions();
    expect(page).toEqual({ sessions: [], nextCursor: "" });
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("sends the page size, cursor, and q parameters with the bearer token", async () => {
    tokenStore("tok");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        jsonResponse({ sessions: [{ id: "s1", title: "t", updatedAt: "now" }], nextCursor: "c2" }),
      ),
    );
    const page = await listSessions("c1", "财务");
    expect(page).not.toBeNull();
    expect(page!.nextCursor).toBe("c2");
    const [url, init] = vi.mocked(fetch).mock.calls[0] as [string, RequestInit];
    expect(url).toContain("limit=25");
    expect(url).toContain("cursor=c1");
    expect(url).toContain("q=");
    expect(init.headers).toMatchObject({ authorization: "Bearer tok" });
  });

  it("returns null on a network rejection, not an empty list", async () => {
    tokenStore("tok");
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")));
    expect(await listSessions()).toBeNull();
  });

  it("returns null on a non-OK response", async () => {
    tokenStore("tok");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({}, 500)));
    expect(await listSessions()).toBeNull();
  });

  it("normalizes a bare/malformed page into empty sessions + cursor", async () => {
    tokenStore("tok");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({})));
    expect(await listSessions()).toEqual({ sessions: [], nextCursor: "" });
  });
});

describe("deleteSession", () => {
  it("resolves false without a token", async () => {
    tokenStore(null);
    vi.stubGlobal("fetch", vi.fn());
    expect(await deleteSession("s1")).toBe(false);
  });

  it("resolves true on a 2xx and false on failure", async () => {
    tokenStore("tok");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    expect(await deleteSession("s1")).toBe(true);
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 404 }));
    expect(await deleteSession("s1")).toBe(false);
  });
});
