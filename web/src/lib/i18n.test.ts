// i18n unit tests: dictionary lookup, placeholder substitution, the language
// switch, and the defensive runtime fallback. detectLang runs at import time
// against the (node) environment and settles on "en", so every test pins its
// language explicitly via setLang.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { getLang, isZh, setLang, t, type I18nKey } from "@/lib/i18n";

const stored = new Map<string, string>();

beforeEach(() => {
  stored.clear();
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => stored.get(k) ?? null,
    setItem: (k: string, v: string) => stored.set(k, v),
    removeItem: (k: string) => stored.delete(k),
  });
  setLang("en");
});

describe("t()", () => {
  it("returns the active dictionary's string", () => {
    setLang("zh");
    expect(t("chat.new")).toBe("新建对话");
    setLang("en");
    expect(t("chat.new")).toBe("New chat");
  });

  it("substitutes {name} placeholders", () => {
    setLang("zh");
    expect(t("chat.noMatchesHint", { term: "财务" })).toBe("没有与「财务」匹配的内容。");
    setLang("en");
    expect(t("chat.noMatchesHint", { term: "x" })).toBe("Nothing matches “x”.");
  });

  it("renders the key itself when the active dict lacks it (defensive fallback)", () => {
    // I18nKey is balanced by construction; exercise the runtime guard via a cast.
    const unknown = "brand.newKey" as I18nKey;
    expect(t(unknown)).toBe("brand.newKey");
  });
});

describe("language switching", () => {
  it("setLang persists the choice and getLang/isZh follow it", () => {
    expect(getLang()).toBe("en");
    expect(isZh()).toBe(false);
    setLang("zh");
    expect(getLang()).toBe("zh");
    expect(isZh()).toBe(true);
    expect(stored.get("nowhere.lang")).toBe("zh");
  });

  it("setLang to the current language is a no-op", () => {
    setLang("en");
    expect(getLang()).toBe("en");
  });
});
