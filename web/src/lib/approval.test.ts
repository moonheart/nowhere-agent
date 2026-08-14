// Approval-bus unit tests, DOM-free: the epoch guard, the pending map
// lifecycle, and the client_tool auto-run round-trip (which only touches
// storage + fetch, both stubbed). The failed-map and pending-queue reads
// are React-hook only (useApprovalFailure / usePendingInteractions) and are
// left to component-level testing.

import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  approvalEpoch,
  clearApproval,
  hasPendingInteractions,
  reportInteraction,
  resetApprovals,
  type Interaction,
} from "@/lib/approval";

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

function interaction(over: Partial<Interaction> = {}): Interaction {
  return {
    interactionId: "i1",
    toolCallId: "tc1",
    toolName: "sleep",
    ...over,
  };
}

beforeEach(() => {
  resetApprovals();
  vi.unstubAllGlobals();
});

describe("epoch guard", () => {
  it("drops frames tagged with a stale epoch", () => {
    reportInteraction(interaction(), approvalEpoch() - 1);
    expect(hasPendingInteractions()).toBe(false);
  });

  it("accepts frames from the current epoch", () => {
    reportInteraction(interaction());
    expect(hasPendingInteractions()).toBe(true);
  });

  it("resetApprovals bumps the epoch so in-flight frames are dropped", () => {
    reportInteraction(interaction());
    resetApprovals();
    expect(approvalEpoch()).toBeGreaterThan(0);
    expect(hasPendingInteractions()).toBe(false);
    reportInteraction(interaction());
    expect(hasPendingInteractions()).toBe(true);
  });
});

describe("pending lifecycle", () => {
  it("clearApproval removes a reported interaction", () => {
    reportInteraction(interaction());
    expect(hasPendingInteractions()).toBe(true);
    clearApproval("tc1");
    expect(hasPendingInteractions()).toBe(false);
  });

  it("clearApproval on an unknown id is a no-op", () => {
    clearApproval("nope");
  });

  it("drops frames without an id or toolCallId", () => {
    reportInteraction(interaction({ interactionId: "" }));
    reportInteraction(interaction({ toolCallId: "" }));
    expect(hasPendingInteractions()).toBe(false);
  });
});

describe("client_tool auto-run", () => {
  it("executes the capability and clears the prompt on success", async () => {
    vi.stubGlobal("localStorage", memStorage());
    vi.stubGlobal("sessionStorage", memStorage({ "nowhere.token": "tok" }));
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, status: 200, body: new Response(new Uint8Array(1)).body }),
    );
    reportInteraction(interaction({ kind: "client_tool", args: { seconds: 0 } }));
    expect(hasPendingInteractions()).toBe(true);
    await vi.waitFor(() => expect(hasPendingInteractions()).toBe(false));
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("keeps the prompt when no token can send the verdict", async () => {
    vi.stubGlobal("localStorage", memStorage());
    vi.stubGlobal("sessionStorage", memStorage());
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, status: 200, body: null }),
    );
    reportInteraction(interaction({ kind: "client_tool", args: { seconds: 0 } }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(hasPendingInteractions()).toBe(true);
    expect(fetch).not.toHaveBeenCalled();
  });
});
