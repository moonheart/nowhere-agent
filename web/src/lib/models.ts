// Chat model picker (chat-side model selection). GET /api/chat/models lists
// the enabled models of the provider the caller's chat runs resolve to (team
// assignment → platform default, resolved per request server-side); the
// composer dropdown writes the chosen name into the chat POST body's `model`
// field ("" = the server's resolved default; a stale picker never breaks chat,
// the backend falls back). Empty list = no servable provider, and the picker
// hides. Module-level (outside assistant-ui) so the composer in thread.tsx and
// the runtime body in App.tsx share one selection.

import { useSyncExternalStore } from "react";
import { api } from "@/lib/api";

export type ModelChoices = {
  /** The model the caller's chat runs resolve to when no override is sent. */
  default: string;
  /** Enabled model names on the resolved provider (empty = picker hidden). */
  models: string[];
  /** The model the next send carries ("" = server default). */
  selected: string;
};

let state: ModelChoices = { default: "", models: [], selected: "" };
let loading = false;
let loaded = false;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function getSnapshot(): ModelChoices {
  return state;
}

// useModelChoices subscribes to the picker state (default + enabled models +
// current selection).
export function useModelChoices(): ModelChoices {
  return useSyncExternalStore(subscribe, getSnapshot);
}

// selectedModel returns the name the next send carries ("" = server default).
// Called from the chat POST body at send time, so it reads the live selection.
export function selectedModel(): string {
  return state.selected;
}

// ensureModels loads the picker once per page load. Called from the composer
// mount (and awaited by tests); a failure (or no servable provider) leaves the
// list empty and the picker hidden — chat never depends on it.
export function ensureModels(): Promise<void> {
  if (loaded || loading) return Promise.resolve();
  loading = true;
  return api<{ default?: string; models?: unknown }>("/api/chat/models")
    .then((m) => {
      if (!m || !Array.isArray(m.models)) return;
      const names = m.models.filter(
        (n): n is string => typeof n === "string" && n.length > 0,
      );
      const def = typeof m.default === "string" ? m.default : "";
      state = {
        default: def,
        models: names,
        selected: state.selected || def,
      };
    })
    .catch(() => {
      // A picker outage must never break chat: leave the list empty.
    })
    .finally(() => {
      loaded = true;
      loading = false;
      emit();
    });
}

// selectModel sets the model the next send carries ("" = server default).
export function selectModel(name: string): void {
  if (state.selected === name) return;
  state = { ...state, selected: name };
  emit();
}

// resetModelStore clears the cached picker (tests only: forces the next
// ensureModels to re-fetch).
export function resetModelStore(): void {
  state = { default: "", models: [], selected: "" };
  loaded = false;
  loading = false;
  emit();
}
