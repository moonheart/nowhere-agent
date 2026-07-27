import type { FC } from "react";
import {
  ThreadPrimitive,
  ComposerPrimitive,
  MessagePrimitive,
} from "@assistant-ui/react";
import { ArrowUp } from "lucide-react";
import { Reasoning } from "@/components/reasoning";
import { StopButton } from "@/components/stop-button";
import { ToolCall } from "@/components/tool-call";
import { MarkdownText } from "@/components/markdown-text";
import { UsageFooter } from "@/components/usage-footer";
import { PlanPanel } from "@/components/plan-panel";

export const Thread: FC = () => {
  return (
    <ThreadPrimitive.Root className="flex h-full flex-col bg-white">
      <PlanPanel />
      <ThreadPrimitive.Viewport className="flex flex-1 flex-col gap-5 overflow-y-auto px-6 py-6">
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

      <Composer />
    </ThreadPrimitive.Root>
  );
};

const EmptyState: FC = () => (
  <div className="flex h-full flex-col items-center justify-center gap-3 text-center">
    <span className="flex h-12 w-12 items-center justify-center rounded-2xl bg-violet-600 text-lg font-bold text-white">
      n
    </span>
    <div>
      <p className="text-base font-semibold text-neutral-800">
        How can I help?
      </p>
      <p className="mt-1 text-sm text-neutral-400">
        Ask anything, or have me work with files in your workspace.
      </p>
    </div>
  </div>
);

const UserMessage: FC = () => (
  <MessagePrimitive.Root className="flex justify-end">
    <div className="max-w-[75%] rounded-2xl rounded-br-md bg-violet-600 px-4 py-2.5 text-white shadow-sm">
      <MessagePrimitive.Parts />
    </div>
  </MessagePrimitive.Root>
);

const AssistantMessage: FC = () => (
  <MessagePrimitive.Root className="flex justify-start">
    <div className="max-w-[80%] rounded-2xl rounded-bl-md border border-neutral-200 bg-neutral-50 px-4 py-3 text-neutral-900">
      <MessagePrimitive.Parts
        components={{
          Text: MarkdownText,
          Reasoning,
          tools: { Fallback: ToolCall },
        }}
      />
      <UsageFooter />
    </div>
  </MessagePrimitive.Root>
);

const Composer: FC = () => (
  <div className="border-t border-neutral-200 bg-white p-4">
    <ComposerPrimitive.Root className="mx-auto flex max-w-3xl items-end gap-2 rounded-2xl border border-neutral-300 bg-white px-3 py-2 shadow-sm transition-colors focus-within:border-violet-400 focus-within:ring-2 focus-within:ring-violet-100">
      <ComposerPrimitive.Input
        placeholder="Message nowhere-agent…"
        className="max-h-40 flex-1 resize-none bg-transparent px-1 py-1.5 text-[15px] outline-none placeholder:text-neutral-400"
      />
      <ThreadPrimitive.If running={false}>
        <ComposerPrimitive.Send
          title="Send"
          className="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-violet-600 text-white transition-colors hover:bg-violet-700 disabled:opacity-40"
        >
          <ArrowUp size={18} />
        </ComposerPrimitive.Send>
      </ThreadPrimitive.If>
      <ThreadPrimitive.If running>
        <StopButton className="flex h-9 shrink-0 items-center rounded-xl bg-neutral-200 px-4 text-sm font-medium text-neutral-800 transition-colors hover:bg-neutral-300" />
      </ThreadPrimitive.If>
    </ComposerPrimitive.Root>
    <p className="mx-auto mt-2 max-w-3xl text-center text-[11px] text-neutral-400">
      nowhere-agent can read and write files in your workspace. Double-check
      important output.
    </p>
  </div>
);
