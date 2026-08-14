// image-attachment unit tests: the pending-attachment list lifecycle and the
// URL/placeholder pure helpers. The authenticated-image hook (useAuthenticatedImage)
// needs a browser fetch + object-URL environment, so it is left to component
// testing; the shared cache logic is exercised here only via its pure inputs.

import { beforeEach, describe, expect, it } from "vitest";
import {
  BROKEN_IMAGE_SRC,
  addImage,
  imageFileUrl,
  removeImage,
  resetImages,
  takeImages,
} from "@/lib/image-attachment";

beforeEach(() => {
  resetImages();
});

describe("pending attachment lifecycle", () => {
  it("addImage appends and dedupes by path", () => {
    addImage({ path: "a.webp", mediaType: "image/webp", name: "a.png" });
    addImage({ path: "a.webp", mediaType: "image/webp", name: "dup" });
    addImage({ path: "b.webp", mediaType: "image/webp", name: "b.png" });
    expect(takeImages()).toEqual([
      { path: "a.webp", mediaType: "image/webp", name: "a.png" },
      { path: "b.webp", mediaType: "image/webp", name: "b.png" },
    ]);
  });

  it("removeImage drops only the named chip", () => {
    addImage({ path: "a.webp", mediaType: "image/webp", name: "a.png" });
    addImage({ path: "b.webp", mediaType: "image/webp", name: "b.png" });
    removeImage("a.webp");
    const batch = takeImages();
    expect(batch).toHaveLength(1);
    expect(batch[0].path).toBe("b.webp");
  });

  it("takeImages is exactly-once: the batch is cleared after one read", () => {
    addImage({ path: "a.webp", mediaType: "image/webp", name: "a.png" });
    expect(takeImages()).toHaveLength(1);
    expect(takeImages()).toEqual([]);
  });

  it("takeImages on an empty list returns [] without emitting", () => {
    expect(takeImages()).toEqual([]);
  });

  it("resetImages clears staged chips (conversation switch)", () => {
    addImage({ path: "a.webp", mediaType: "image/webp", name: "a.png" });
    resetImages();
    expect(takeImages()).toEqual([]);
  });
});

describe("imageFileUrl", () => {
  it("maps user-level uploads to the chat upload endpoint, id-escaped", () => {
    expect(imageFileUrl(null, "uploads/abc.webp")).toBe("/api/chat/uploads/abc.webp");
    expect(imageFileUrl(null, "uploads/we%20ird.webp")).toBe("/api/chat/uploads/we%2520ird.webp");
  });

  it("maps session-relative paths to the session files endpoint", () => {
    expect(imageFileUrl("s1", "img1.webp")).toBe("/api/chat/sessions/s1/files/img1.webp");
    expect(imageFileUrl(null, "img1.webp")).toBe("/api/chat/sessions//files/img1.webp");
  });
});

describe("BROKEN_IMAGE_SRC", () => {
  it("is a self-contained data URI (no token, no object URL)", () => {
    expect(BROKEN_IMAGE_SRC.startsWith("data:image/svg+xml;utf8,")).toBe(true);
    expect(decodeURIComponent(BROKEN_IMAGE_SRC)).toContain("<svg");
  });
});
