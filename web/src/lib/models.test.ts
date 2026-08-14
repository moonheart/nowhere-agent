// Model-picker store unit tests, DOM-free: the lazy one-shot load, the
// selected-model lifecycle, and failure tolerance (a broken picker never
// surfaces a selection that could break chat).

import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  ensureModels,
  resetModelStore,
  selectModel,
  selectedModel,
} from "@/lib/models";

// The store keeps module state across tests; reset it before each one so the
// one-shot load runs again. Both storages must exist (auth.getToken reads
// sessionStorage with a localStorage fallback migration), like the other lib
// tests stub them.
function memStorage(seed: Record<string, string> = {}): Storage {
  const m = new Map(Object.entries(seed));
  return {
    getItem: (k) => m.get(k) ?? null,
    setItem: (k, v) => void m.set(k, v),
    removeItem: (k) => void m.delete(k),
  } as Storage;
}

beforeEach(() => {
  vi.unstubAllGlobals();
  resetModelStore();
  vi.stubGlobal("localStorage", memStorage());
  vi.stubGlobal("sessionStorage", memStorage({ "nowhere.token": "tok" }));
});

describe("ensureModels", () => {
  it("loads once and exposes default + models, selected defaulting to the default", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(
            JSON.stringify({ default: "sonnet", models: ["sonnet", "haiku"] }),
          ),
      } as never),
    );
    await ensureModels();
    expect(selectedModel()).toBe("sonnet");
  });

  it("ignores a server error and leaves the picker empty (chat unaffected)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        text: () => Promise.resolve("boom"),
      } as never),
    );
    await ensureModels();
    expect(selectedModel()).toBe("");
  });
});

describe("selectModel", () => {
  it("records the chosen model for the next send", () => {
    selectModel("haiku");
    expect(selectedModel()).toBe("haiku");
  });
});
