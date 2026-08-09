// Pending image attachments for the current composer turn (image-input
// capability). The composer uploads each selected image to the session first,
// then holds its returned path here as a chip. When the user sends, App.tsx's
// chat POST body reads the batch via takeImages() (clearing it) and carries it
// as top-level `images:[{path, mediaType}]`; the backend appends them as
// BlockImage to the user turn. Module-level (outside assistant-ui) so the
// composer in thread.tsx and the runtime body in App.tsx share one list.

import { useSyncExternalStore } from "react";

export type PendingImage = {
  /** Session-relative path returned by the upload endpoint (…/images). */
  path: string;
  /** Uploaded media type; the store normalizes the file to WebP. */
  mediaType: string;
  /** Original file name, shown on the attachment chip. */
  name: string;
};

let pending: PendingImage[] = [];
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function getSnapshot(): PendingImage[] {
  return pending;
}

// usePendingImages subscribes to the current turn's attachment chips.
export function usePendingImages(): PendingImage[] {
  return useSyncExternalStore(subscribe, getSnapshot);
}

// addImage records one uploaded image so it renders as a chip and rides the
// next send.
export function addImage(img: PendingImage) {
  if (pending.some((p) => p.path === img.path)) return;
  pending = [...pending, img];
  emit();
}

// removeImage drops a chip (the user un-attaches an image before sending).
export function removeImage(path: string) {
  if (!pending.some((p) => p.path === path)) return;
  pending = pending.filter((p) => p.path !== path);
  emit();
}

// takeImages returns the current batch and clears it. Called from the chat POST
// body so a send carries (exactly once) the images chosen for that turn.
export function takeImages(): PendingImage[] {
  if (pending.length === 0) return [];
  const out = pending;
  pending = [];
  emit();
  return out;
}

// resetImages drops any staged images on conversation switch, so chips can't
// leak into a different session's next send.
export function resetImages() {
  if (pending.length === 0) return;
  pending = [];
  emit();
}

// imageFileUrl resolves an image reference to the authenticated endpoint the
// browser loads it from. A "uploads/<id>.webp" reference is a user-level
// upload (change user-image-uploads) served from /api/chat/uploads/<id>.webp,
// scoped to the signed-in user; anything else is a session-relative workspace
// image served from GET .../sessions/{id}/files/{path}. Both forms come from
// their upload endpoints and are safe to interpolate (a uuid + extension).
export function imageFileUrl(sessionId: string | null, path: string): string {
  if (path.startsWith("uploads/")) {
    return `/api/chat/uploads/${encodeURIComponent(path.slice("uploads/".length))}`;
  }
  return `/api/chat/sessions/${encodeURIComponent(sessionId ?? "")}/files/${path}`;
}
