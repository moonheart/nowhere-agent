// api() watchdog-timeout tests, DOM-free: fetch is stubbed to either never
// settle (timeout path) or reject on abort (caller-signal path), and the
// watchdog fires under fake timers.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/lib/api";

// neverSettlingFetch resolves nothing; it rejects only when its signal aborts,
// mirroring what real fetch does on abort.
function neverSettlingFetch(_url: string, init?: { signal?: AbortSignal }) {
  return new Promise((_resolve, reject) => {
    init?.signal?.addEventListener(
      "abort",
      () => reject(new DOMException("aborted", "AbortError")),
      { once: true },
    );
  });
}

beforeEach(() => {
  vi.stubGlobal("localStorage", { getItem: () => null });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("api() watchdog timeout", () => {
  it("aborts a never-settling request after the timeout and throws ApiError(408)", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", vi.fn(neverSettlingFetch));

    const p = api("/x");
    const rejection = p.then(
      () => {
        throw new Error("expected rejection");
      },
      (e: unknown) => e,
    );
    await vi.advanceTimersByTimeAsync(60_000);
    const err = await rejection;
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(408);
    expect((err as ApiError).message).toBe("request timed out");
  });

  it("passes a caller-provided signal through untouched (no watchdog conversion)", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", vi.fn(neverSettlingFetch));

    const controller = new AbortController();
    const p = api("/x", { signal: controller.signal });
    controller.abort();
    await expect(p).rejects.toThrow("aborted");
    // The watchdog must stay silent for caller-owned signals even after its
    // interval elapses.
    await vi.advanceTimersByTimeAsync(60_000);
  });

  it("resolves normally before the timeout", async () => {
    vi.useFakeTimers();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: async () => '{"ok":true}',
      }),
    );

    const p = api<{ ok: boolean }>("/x");
    await expect(p).resolves.toEqual({ ok: true });
  });
});
