// Pending image attachments for the current composer turn (image-input
// capability). The composer uploads each selected image to the session first,
// then holds its returned path here as a chip. When the user sends, App.tsx's
// chat POST body reads the batch via takeImages() (clearing it) and carries it
// as top-level `images:[{path, mediaType}]`; the backend appends them as
// BlockImage to the user turn. Module-level (outside assistant-ui) so the
// composer in thread.tsx and the runtime body in App.tsx share one list.

import { useEffect, useState } from "react";
import { useSyncExternalStore } from "react";
import { getToken } from "@/lib/auth";

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

// imageFileUrl resolves an image reference to the endpoint the browser loads
// it from. A "uploads/<id>.webp" reference is a user-level upload (change
// user-image-uploads) served from /api/chat/uploads/<id>.webp, scoped to the
// signed-in user; anything else is a session-relative workspace image served
// from GET .../sessions/{id}/files/{path}. Both forms come from their upload
// endpoints and are safe to interpolate (a uuid + extension).
//
// The URL deliberately carries NO token. An <img> tag cannot set an
// Authorization header, so renderers resolve the bytes through an
// authenticated fetch (useAuthenticatedImage) instead: a bearer token in the
// query string would leak into proxy access logs and browser history.
export function imageFileUrl(sessionId: string | null, path: string): string {
  if (path.startsWith("uploads/")) {
    return `/api/chat/uploads/${encodeURIComponent(path.slice("uploads/".length))}`;
  }
  return `/api/chat/sessions/${encodeURIComponent(sessionId ?? "")}/files/${path}`;
}

// useAuthenticatedImage fetches a server image URL with the bearer token — an
// <img> tag cannot set an Authorization header — and returns a blob URL to
// render. The URL itself stays token-free (see imageFileUrl): the token never
// rides the query string into proxy access logs or browser history. Returns
// undefined while the bytes are loading or on failure. Blob URLs are revoked
// when the src changes or the component unmounts.
export function useAuthenticatedImage(src: string): string | undefined {
  const [blob, setBlob] = useState<string | undefined>(undefined);

  useEffect(() => {
    let revoked = false;
    let url: string | undefined;
    setBlob(undefined);
    const token = getToken();
    if (!src || !token) return;

    const ctrl = new AbortController();
    (async () => {
      try {
        const res = await fetch(src, {
          headers: { authorization: `Bearer ${token}` },
          signal: ctrl.signal,
        });
        if (!res.ok) return;
        const bytes = await res.blob();
        if (revoked) return;
        url = URL.createObjectURL(bytes);
        if (revoked) {
          URL.revokeObjectURL(url);
          return;
        }
        setBlob(url);
      } catch {
        // Aborted by cleanup or network failure: render nothing.
      }
    })();

    return () => {
      revoked = true;
      ctrl.abort();
      if (url) URL.revokeObjectURL(url);
    };
  }, [src]);

  return blob;
}
