import type { ChangeEvent, FC } from "react";
import { useRef, useState } from "react";
import {
  ThreadPrimitive,
  ComposerPrimitive,
  MessagePrimitive,
} from "@assistant-ui/react";
import { ArrowDown, ArrowUp, Loader2, Paperclip, X } from "lucide-react";
import { Reasoning } from "@/components/reasoning";
import { StopButton } from "@/components/stop-button";
import { ToolCall } from "@/components/tool-call";
import { MarkdownText } from "@/components/markdown-text";
import { UsageFooter } from "@/components/usage-footer";
import { PlanPanel } from "@/components/plan-panel";
import { PendingNotice } from "@/components/pending-notice";
import { Bubble, BubbleContent } from "@/components/ui/bubble";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { InputGroup, InputGroupAddon } from "@/components/ui/input-group";
import { Message } from "@/components/ui/message";
import {
  Attachment,
  AttachmentAction,
  AttachmentActions,
  AttachmentContent,
  AttachmentDescription,
  AttachmentGroup,
  AttachmentMedia,
  AttachmentTitle,
} from "@/components/ui/attachment";
import { PermissionSelect } from "@/components/permission-select";
import { uploadSessionImage } from "@/lib/api";
import {
  addImage,
  imageFileUrl,
  removeImage,
  usePendingImages,
} from "@/lib/image-attachment";
import { reportNotice } from "@/lib/notice";

export const Thread: FC<{ sessionId: string | null }> = ({ sessionId }) => {
  return (
    <ThreadPrimitive.Root className="flex h-full flex-col bg-background">
      <PlanPanel />
      <PendingNotice />
      {/* This wrapper exists only so the scroll-to-bottom button can be
          positioned against the viewport without living inside it — a child of
          the scroller would scroll away with the messages. min-h-0 because the
          wrapper, unlike the viewport, has no overflow to zero out its
          automatic flex minimum. */}
      <div className="relative flex min-h-0 flex-1 flex-col">
        {/* scroll-fade-b masks the bottom edge while there is more below. It is
            driven by animation-timeline: scroll(self y), so the fade retracts on
            its own as you reach the end; browsers without scroll-driven
            animations get shadcn's static fallback (always faded). */}
        <ThreadPrimitive.Viewport className="flex flex-1 flex-col gap-5 overflow-y-auto scroll-fade-b px-6 py-6">
          <ThreadPrimitive.Empty>
            <EmptyState />
          </ThreadPrimitive.Empty>

          <ThreadPrimitive.Messages
            components={{
              UserMessage,
              AssistantMessage,
            }}
          />
        </ThreadPrimitive.Viewport>

        <ScrollToBottom />
      </div>

      <Composer sessionId={sessionId} />
    </ThreadPrimitive.Root>
  );
};

// Styled after shadcn's MessageScrollerButton, but driven by assistant-ui's
// viewport store: ScrollToBottom hands us disabled=true whenever isAtBottom, so
// the whole show/hide transition keys off :disabled rather than app state.
const ScrollToBottom: FC = () => (
  <ThreadPrimitive.ScrollToBottom asChild>
    <Button
      variant="outline"
      size="icon-sm"
      title="Scroll to bottom"
      className="absolute bottom-4 left-1/2 -translate-x-1/2 rounded-full shadow-xs transition-[translate,scale,opacity] duration-200 disabled:pointer-events-none disabled:translate-y-full disabled:scale-95 disabled:opacity-0"
    >
      <ArrowDown />
      <span className="sr-only">Scroll to bottom</span>
    </Button>
  </ThreadPrimitive.ScrollToBottom>
);

const EmptyState: FC = () => (
  <Empty className="h-full">
    <EmptyHeader>
      <EmptyMedia className="mb-2 size-12 rounded-2xl bg-primary text-lg font-bold text-primary-foreground">
        n
      </EmptyMedia>
      <EmptyTitle className="text-base">How can I help?</EmptyTitle>
      <EmptyDescription>
        Ask anything, or have me work with files in your workspace.
      </EmptyDescription>
    </EmptyHeader>
  </Empty>
);

// Deliberately NOT wearing shadcn MessageScrollerItem's
// `content-visibility: auto` + `contain-intrinsic-size: auto 10rem`. That pair
// assumes uniformly short chat bubbles; our assistant turns carry collapsible
// tool panels and code blocks and run 50px–1100px+. Measured on a 19-message
// thread whose true height is 6056px, the skipped turns reported the 180px
// placeholder instead and the viewport claimed 2564–4165px — a wrong scrollbar
// and up to 267px of drift per scroll step. Without it, scrollHeight held at
// 6056px across every sample.
// asChild merges the primitive's props (hover tracking, turn anchoring) onto the
// shadcn Message, so the runtime keeps its behaviour and the markup stays the
// library's — no wrapper div between the two.
const UserMessage: FC = () => (
  <MessagePrimitive.Root asChild>
    <Message align="end">
      <Bubble align="end">
        <BubbleContent className="px-4 py-2.5">
          <MessagePrimitive.Parts />
        </BubbleContent>
      </Bubble>
    </Message>
  </MessagePrimitive.Root>
);

const AssistantMessage: FC = () => (
  <MessagePrimitive.Root asChild>
    <Message>
      {/* ghost is shadcn's "this turn isn't a bubble": no border, no fill, no
          padding, and max-w-full instead of the 80% a bubble is capped at. The
          assistant's reply reads as page content, and the blocks inside it —
          reasoning, tool calls — keep their own frames instead of nesting a
          box in a box. */}
      <Bubble variant="ghost">
        <BubbleContent>
          <MessagePrimitive.Parts
            components={{
              Text: MarkdownText,
              Reasoning,
              tools: { Fallback: ToolCall },
            }}
          />
          <UsageFooter />
        </BubbleContent>
      </Bubble>
    </Message>
  </MessagePrimitive.Root>
);

const Composer: FC<{ sessionId: string | null }> = ({ sessionId }) => {
  // Staged image attachments for the current turn: chips render here, and the
  // chat POST body (App.tsx) consumes them via takeImages() on send.
  const images = usePendingImages();
  const fileRef = useRef<HTMLInputElement>(null);
  const [uploading, setUploading] = useState(false);

  // Each selected image is uploaded immediately (the backend re-encodes to WebP
  // and returns the session-relative path); the chip then shows the stored file
  // through the same authenticated /files/ endpoint history rendering uses.
  const onFiles = async (e: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(e.target.files ?? []);
    e.target.value = ""; // allow re-selecting the same file
    if (files.length === 0 || !sessionId) return;
    setUploading(true);
    let uploaded = 0;
    for (const file of files) {
      try {
        const { path } = await uploadSessionImage(sessionId, file);
        addImage({ path, mediaType: "image/webp", name: file.name });
        uploaded++;
      } catch {
        reportNotice(`Could not upload ${file.name} — try a different image.`);
      }
    }
    setUploading(false);
    if (uploaded === 0) return;
  };

  return (
    <div className="border-t border-border bg-background p-4">
      {/* Root is a <form>, so it wraps the group rather than replacing it —
          asChild would move onSubmit onto a plain div and the Send button would
          stop working. */}
      <ComposerPrimitive.Root className="mx-auto max-w-3xl">
        {images.length > 0 && sessionId && (
          <AttachmentGroup className="mb-2">
            {images.map((p) => (
              <Attachment key={p.path} size="sm">
                <AttachmentMedia variant="image">
                  <img src={imageFileUrl(sessionId, p.path)} alt={p.name} />
                </AttachmentMedia>
                <AttachmentContent>
                  <AttachmentTitle>{p.name}</AttachmentTitle>
                  <AttachmentDescription>WebP image</AttachmentDescription>
                </AttachmentContent>
                <AttachmentActions>
                  <AttachmentAction
                    title="Remove image"
                    onClick={() => removeImage(p.path)}
                  >
                    <X />
                  </AttachmentAction>
                </AttachmentActions>
              </Attachment>
            ))}
          </AttachmentGroup>
        )}
        {/* has-disabled overrides: InputGroup greys itself out when it contains a
            disabled control, and Send is disabled whenever the composer is empty
            — which is exactly when the composer should look most inviting. */}
        <InputGroup className="has-disabled:bg-transparent has-disabled:opacity-100">
          {/* Deliberately NOT render={<InputGroupTextarea />}: passing `render`
              makes the primitive drop react-textarea-autosize, and shadcn's
              Textarea grows via `field-sizing: content`, which Firefox and Safari
              don't support yet. Styled as the group's control instead, so the
              group still owns the border and focus ring.
              w-full, not flex-1: a block-end addon flips the group to flex-col,
              where flex-1 stretches the box vertically and items-center shrinks
              it to the placeholder's width. */}
          <ComposerPrimitive.Input
            placeholder="Message nowhere-agent…"
            maxRows={8}
            data-slot="input-group-control"
            className="w-full resize-none bg-transparent px-2.5 py-2 text-base outline-none placeholder:text-muted-foreground md:text-sm"
          />
          <InputGroupAddon align="block-end">
            {/* Bottom-left: image attach (new chats have no session to upload
                into until the first message creates one — start with text), then
                the execution-permission selector. Send/Stop carry ml-auto, so
                these stay pinned to the left of them. */}
            <ThreadPrimitive.If running={false}>
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                title={
                  sessionId
                    ? "Attach image"
                    : "Attach images after the first message creates this chat"
                }
                disabled={!sessionId || uploading}
                onClick={() => fileRef.current?.click()}
              >
                {uploading ? (
                  <Loader2 className="size-3.5 shrink-0 animate-spin" />
                ) : (
                  <Paperclip />
                )}
              </Button>
              <input
                ref={fileRef}
                type="file"
                accept="image/*"
                multiple
                className="hidden"
                onChange={onFiles}
              />
            </ThreadPrimitive.If>
            <PermissionSelect sessionId={sessionId} />
            <ThreadPrimitive.If running={false}>
              <ComposerPrimitive.Send asChild>
                <Button size="icon-sm" title="Send" className="ml-auto">
                  <ArrowUp />
                </Button>
              </ComposerPrimitive.Send>
            </ThreadPrimitive.If>
            <ThreadPrimitive.If running>
              <StopButton className="ml-auto" />
            </ThreadPrimitive.If>
          </InputGroupAddon>
        </InputGroup>
      </ComposerPrimitive.Root>
      <p className="mx-auto mt-2 max-w-3xl text-center text-[11px] text-muted-foreground">
        nowhere-agent can read and write files in your workspace. Double-check
        important output.
      </p>
    </div>
  );
};
