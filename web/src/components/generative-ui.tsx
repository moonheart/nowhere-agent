import type { ComponentType, FC, ReactNode } from "react";
import { useMessage } from "@assistant-ui/react";
import type { GenerativeUINode, GenerativeUISpec } from "@assistant-ui/react";
import { cn } from "@/lib/utils";

// Agent-driven UI (generative UI): the backend's test_ui tool declares a JSON
// spec tree; this module resolves component names against ALLOWLIST (the
// security boundary — a name not in the registry renders nothing) and renders
// the tree inside the assistant message. The spec travels as a data part named
// "generative-ui" on both the live stream (data-generative-ui frame) and
// history reloads (same shape), so one renderer covers both.

// TestUiCard is the demo card the test_ui / ui_progress tools push. It renders
// a progress bar when the spec carries a `percent`.
const TestUiCard: FC<{
  title?: string;
  body?: string;
  variant?: string;
  percent?: number;
  stage?: string;
  children?: ReactNode;
}> = ({ title, body, variant, percent, stage, children }) => (
  <div
    className={cn(
      "my-1 rounded-lg border p-3",
      variant === "success"
        ? "border-emerald-600/30 bg-emerald-500/10"
        : variant === "error"
          ? "border-red-600/30 bg-red-500/10"
          : "border-border bg-muted/50",
    )}
  >
    <p className="text-sm font-semibold">{title ?? "Agent UI"}</p>
    {body && <p className="mt-0.5 text-sm text-muted-foreground">{body}</p>}
    {typeof percent === "number" && (
      <div className="mt-2">
        <div className="h-1.5 w-full overflow-hidden rounded-full bg-foreground/10">
          <div
            className="h-full rounded-full bg-primary transition-all duration-300"
            style={{ width: `${Math.min(100, Math.max(0, percent))}%` }}
          />
        </div>
        <p className="mt-1 text-xs text-muted-foreground">
          {percent}%{stage ? ` · ${stage}` : ""}
        </p>
      </div>
    )}
    {children && <div className="mt-2 flex flex-col gap-1">{children}</div>}
  </div>
);

// TestUiBullet is one line of a card's list.
const TestUiBullet: FC<{ text?: string }> = ({ text }) => (
  <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
    <span className="mt-1 size-1 shrink-0 rounded-full bg-current" />
    {text}
  </p>
);

// ALLOWLIST is the registry of components a generative-ui spec may reference.
const ALLOWLIST: Record<string, ComponentType<any>> = {
  "test-ui-card": TestUiCard,
  "test-ui-bullet": TestUiBullet,
};

// NodeRenderer renders one spec node: strings render as text, objects resolve
// the component against the allowlist (unknown names render nothing).
const NodeRenderer: FC<{ node: GenerativeUINode; allowlist: Record<string, ComponentType<any>> }> = ({
  node,
  allowlist,
}) => {
  if (typeof node === "string") return <>{node}</>;
  const Component = allowlist[node.component];
  if (!Component) return null;
  return (
    <Component {...(node.props ?? {})}>
      {node.children?.map((child, i) => (
        <NodeRenderer key={node.key ?? `n${i}`} node={child} allowlist={allowlist} />
      ))}
    </Component>
  );
};

// SpecTree renders a full generative-UI spec.
export const SpecTree: FC<{ spec: GenerativeUISpec; allowlist?: Record<string, ComponentType<any>> }> = ({
  spec,
  allowlist = ALLOWLIST,
}) => {
  const root = Array.isArray(spec.root) ? spec.root : [spec.root];
  return (
    <>
      {root.map((node, i) => (
        <NodeRenderer key={i} node={node} allowlist={allowlist} />
      ))}
    </>
  );
};

// DataUI is the data-part renderer wired into MessagePrimitive.Parts as
// `data: { Fallback: DataUI }`. It renders only the "generative-ui" data part
// and ignores every other data part (usage, etc.). This fires on the HISTORY
// path, where the server rebuilds the spec as a real content part.
export const DataUI: FC<{ name: string; data: unknown }> = ({ name, data }) => {
  if (name !== "generative-ui") return null;
  const spec = (data as { spec?: GenerativeUISpec } | null)?.spec;
  if (!spec) return null;
  return <SpecTree spec={spec} />;
};

// pickGenerativeUISpec finds the NEWEST generative-ui entry carried in a
// message's metadata.unstable_data — where the LIVE data-generative-ui frames
// land: the assistant-stream 0.3.26 accumulator routes non-transient data
// frames into metadata (appending one entry per frame), never into content
// parts (the same channel the data-usage frame and data-session-state use).
// A progress card pushes many frames, so the LAST matching entry wins.
function pickGenerativeUISpec(metadata: unknown): GenerativeUISpec | null {
  const list = (metadata as { unstable_data?: unknown[] } | undefined)
    ?.unstable_data;
  if (!Array.isArray(list)) return null;
  let found: GenerativeUISpec | null = null;
  for (const entry of list) {
    const e = entry as
      | { name?: string; data?: { spec?: GenerativeUISpec } }
      | undefined;
    if (e?.name === "generative-ui" && e.data?.spec) found = e.data.spec;
  }
  return found;
}

// GenerativeUIFromMetadata renders the live-pushed generative UI (from the
// message's metadata.unstable_data), mounted beside UsageFooter in the
// assistant bubble. The spec reference comes straight from the metadata
// object, so the zustand selector stays referentially stable.
export const GenerativeUIFromMetadata: FC = () => {
  const spec = useMessage((s) => pickGenerativeUISpec(s.metadata));
  if (!spec) return null;
  return <SpecTree spec={spec} />;
};
