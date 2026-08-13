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

// BROKEN_IMAGE_SRC is the placeholder shown when an authenticated image fails
// to load: an inline SVG broken-image icon, so a failed fetch is visible
// instead of a permanently blank thumbnail. Data URI needs no object URL, so it
// never leaks and needs no revoke. Consumers may compare the hook's return
// value against it to offer a click-to-retry.
export const BROKEN_IMAGE_SRC =
  "data:image/svg+xml;utf8," +
  encodeURIComponent(
    `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="#9ca3af" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="18" x="3" y="3" rx="2" ry="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21"/></svg>`,
  );

// blobCache shares one object URL across every mount of the same src. Each
// live mount holds a reference (refs); when the last one unmounts or changes
// src, the blob is revoked and the entry dropped — so a long session with many
// mounts of the same images stops re-fetching and re-creating blobs, and the
// memory footprint tracks the distinct images, not the mounts.
type BlobEntry = { url: string; refs: number };
const blobCache = new Map<string, BlobEntry>();

// useAuthenticatedImage fetches a server image URL with the bearer token — an
// <img> tag cannot set an Authorization header — and returns a blob URL to
// render. The URL itself stays token-free (see imageFileUrl): the token never
// rides the query string into proxy access logs or browser history. Returns
// undefined while the bytes are loading, BROKEN_IMAGE_SRC on failure. Bumping
// retry re-fetches a failed src (click-to-retry); a src that already loaded
// (or is loading) is unaffected — retry only matters for the failure path.
export function useAuthenticatedImage(src: string, retry = 0): string | undefined {
  const [blob, setBlob] = useState<string | undefined>(undefined);

  useEffect(() => {
    let revoked = false;
    setBlob(undefined);
    const token = getToken();
    if (!src || !token) return;

    // Another live mount already fetched this src: share its blob URL instead
    // of re-fetching and double-creating an object URL.
    const ctrl = new AbortController();
    const existing = blobCache.get(src);
    if (existing) {
      existing.refs++;
      setBlob(existing.url);
    } else {
      (async () => {
        let url: string | undefined;
        try {
          const res = await fetch(src, {
            headers: { authorization: `Bearer ${token}` },
            signal: ctrl.signal,
          });
          if (!res.ok) throw new Error(`image fetch failed: ${res.status}`);
          const bytes = await res.blob();
          if (revoked) return;
          url = URL.createObjectURL(bytes);
          // Two mounts of the same src raced the fetch; the winner's entry is
          // now in the cache. Join it rather than keeping a duplicate blob.
          const joined = blobCache.get(src);
          if (joined) {
            joined.refs++;
            URL.revokeObjectURL(url);
            setBlob(joined.url);
            return;
          }
          blobCache.set(src, { url, refs: 1 });
          setBlob(url);
        } catch {
          // Aborted by cleanup or a failed/network-broken response: render the
          // placeholder so the failure is visible instead of a blank spot.
          if (!revoked) setBlob(BROKEN_IMAGE_SRC);
        }
      })();
    }

    return () => {
      revoked = true;
      ctrl.abort();
      const cur = blobCache.get(src);
      if (cur && --cur.refs <= 0) {
        blobCache.delete(src);
        URL.revokeObjectURL(cur.url);
      }
    };
  }, [src, retry]);

  return blob;
}
