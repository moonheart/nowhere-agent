import type { FC } from "react";
import {
  ThreadPrimitive,
  ComposerPrimitive,
  MessagePrimitive,
} from "@assistant-ui/react";
import { Reasoning } from "@/components/reasoning";

export const Thread: FC = () => {
  return (
    <ThreadPrimitive.Root className="flex h-full flex-col">
      <ThreadPrimitive.Viewport className="flex flex-1 flex-col gap-4 overflow-y-auto px-4 py-6">
        <ThreadPrimitive.Empty>
          <div className="flex h-full items-center justify-center text-neutral-400">
            Start a conversation.
          </div>
        </ThreadPrimitive.Empty>

        <ThreadPrimitive.Messages
          components={{
            UserMessage,
            AssistantMessage,
          }}
        />
      </ThreadPrimitive.Viewport>

      <Composer />
    </ThreadPrimitive.Root>
  );
};

const UserMessage: FC = () => (
  <MessagePrimitive.Root className="flex justify-end">
    <div className="max-w-[80%] rounded-2xl bg-violet-600 px-4 py-2 text-white">
      <MessagePrimitive.Parts />
    </div>
  </MessagePrimitive.Root>
);

const AssistantMessage: FC = () => (
  <MessagePrimitive.Root className="flex justify-start">
    <div className="max-w-[80%] rounded-2xl bg-neutral-100 px-4 py-2 text-neutral-900">
      <MessagePrimitive.Parts components={{ Reasoning }} />
    </div>
  </MessagePrimitive.Root>
);

const Composer: FC = () => (
  <ComposerPrimitive.Root className="flex items-end gap-2 border-t border-neutral-200 p-4">
    <ComposerPrimitive.Input
      placeholder="Message nowhere-agent…"
      className="max-h-40 flex-1 resize-none rounded-xl border border-neutral-300 px-3 py-2 outline-none focus:border-violet-500"
    />
    <ThreadPrimitive.If running={false}>
      <ComposerPrimitive.Send className="rounded-xl bg-violet-600 px-4 py-2 text-white disabled:opacity-40">
        Send
      </ComposerPrimitive.Send>
    </ThreadPrimitive.If>
    <ThreadPrimitive.If running>
      <ComposerPrimitive.Cancel className="rounded-xl bg-neutral-200 px-4 py-2 text-neutral-800">
        Stop
      </ComposerPrimitive.Cancel>
    </ThreadPrimitive.If>
  </ComposerPrimitive.Root>
);
