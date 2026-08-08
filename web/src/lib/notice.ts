import { useSyncExternalStore } from "react";

// notice is a one-slot ephemeral banner (currently: the pending-interaction
// 409). Mirrors the approval.ts store pattern: module state + listeners, read
// reactively via useNotice.
let notice: string | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const fn of listeners) fn();
}

// reportNotice shows (or replaces) the banner text.
export function reportNotice(text: string) {
  notice = text;
  emit();
}

// clearNotice dismisses the banner. Session switches and new chats call this
// alongside the other store resets.
export function clearNotice() {
  if (notice === null) return;
  notice = null;
  emit();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function getSnapshot(): string | null {
  return notice;
}

// useNotice returns the current banner text, or null when nothing is showing.
export function useNotice(): string | null {
  return useSyncExternalStore(subscribe, getSnapshot);
}
