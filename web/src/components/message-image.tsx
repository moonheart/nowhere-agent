import { useEffect, useState, type FC } from "react";
import { createPortal } from "react-dom";
import { ZoomIn } from "lucide-react";
import type { ImageMessagePartComponent } from "@assistant-ui/react";
import {
  BROKEN_IMAGE_SRC,
  useAuthenticatedImage,
} from "@/lib/image-attachment";

// AuthenticatedImg is a plain <img> whose src is resolved through the
// authenticated fetch — for image tags outside message parts (the composer's
// attachment chip), which have no fetch hook of their own. A failed load shows
// the broken-image placeholder; clicking it retries the fetch.
export const AuthenticatedImg: FC<{
  src: string;
  alt?: string;
  className?: string;
}> = ({ src, alt, className }) => {
  const [retry, setRetry] = useState(0);
  const url = useAuthenticatedImage(src, retry);
  if (url === BROKEN_IMAGE_SRC) {
    return (
      <button
        type="button"
        onClick={() => setRetry((r) => r + 1)}
        title="Retry image"
        aria-label="Retry loading image"
        className={className}
      >
        <img src={BROKEN_IMAGE_SRC} alt={alt} className={className} />
      </button>
    );
  }
  return <img src={url} alt={alt} className={className} />;
};

// MessageImage renders a message's image part as a small square thumbnail;
// clicking it opens a full-size lightbox over the chat. The thumbnail keeps
// image-heavy threads scannable — the enlarged view is one click away.
// (Modeled on assistant-ui's ImageZoom, but thumbnail-first instead of a
// capped preview, and driven by a plain portal rather than the base-ui dialog
// so the lightbox has no focus-trap/title ceremony.)
export const MessageImage: ImageMessagePartComponent = ({ image, filename }) => {
  const [open, setOpen] = useState(false);
  const [retry, setRetry] = useState(0);
  const src = useAuthenticatedImage(image, retry);

  useEffect(() => {
    if (!open) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("keydown", onKeyDown);
    const originalOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      document.body.style.overflow = originalOverflow;
    };
  }, [open]);

  const alt = filename || "Image";
  // A failed load is visible (broken placeholder) and clicking it retries the
  // fetch instead of zooming a blank image.
  const broken = src === BROKEN_IMAGE_SRC;
  const onThumbClick = () => {
    if (broken) setRetry((r) => r + 1);
    else setOpen(true);
  };

  return (
    <>
      <button
        type="button"
        onClick={onThumbClick}
        title={broken ? "Retry image" : "Click to enlarge"}
        aria-label={broken ? "Retry loading image" : alt}
        className="group relative mt-1 shrink-0 cursor-zoom-in rounded-lg border border-border p-0.5 transition-shadow hover:ring-2 hover:ring-ring focus-visible:ring-2 focus-visible:ring-ring"
      >
        <img
          src={src}
          alt={alt}
          className="size-16 rounded-md object-cover"
        />
        {!broken && (
          <span className="absolute right-1.5 bottom-1.5 rounded-full bg-black/50 p-0.5 text-background opacity-0 transition-opacity group-hover:opacity-100">
            <ZoomIn className="size-3" />
          </span>
        )}
      </button>
      {!broken &&
        open &&
        createPortal(
          <div
            role="dialog"
            aria-modal="true"
            aria-label={alt}
            className="fixed inset-0 z-50 flex cursor-zoom-out items-center justify-center bg-black/80 p-4 duration-100 animate-in fade-in-0"
            onClick={() => setOpen(false)}
          >
            <img
              src={src}
              alt={alt}
              className="max-h-[90vh] max-w-[90vw] rounded-lg object-contain duration-100 animate-in zoom-in-95"
            />
          </div>,
          document.body,
        )}
    </>
  );
};

// ImageThumb is the same thumbnail+lightbox for images that arrive outside the
// message-parts machinery (the composer's optimistic attachment mirror).
export const ImageThumb: FC<{ src: string; alt?: string }> = ({ src, alt }) => (
  <MessageImage type="image" image={src} filename={alt} status={{ type: "complete" }} />
);
