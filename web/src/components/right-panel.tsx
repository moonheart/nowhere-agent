import { useMemo, useState, type FC } from "react";
import {
  FolderTree,
  Activity,
  Brain,
  Blocks,
  File,
  FileCode2,
  FileText,
  CheckCircle2,
  XCircle,
  Loader2,
  ChevronRight,
  type LucideIcon,
} from "lucide-react";
import { useActivity, type ToolActivity } from "@/lib/activity";

type TabId = "workspace" | "runs" | "memory" | "skills";

const TABS: { id: TabId; label: string; icon: LucideIcon }[] = [
  { id: "workspace", label: "Workspace", icon: FolderTree },
  { id: "runs", label: "Runs", icon: Activity },
  { id: "memory", label: "Memory", icon: Brain },
  { id: "skills", label: "Skills", icon: Blocks },
];

/**
 * Right-hand inspector panel. Four tabs: Workspace (files the agent touched),
 * Runs (tool-call log), Memory and Skills (placeholders for upcoming backend
 * features). Data comes from the in-app activity feed the thread publishes.
 */
export const RightPanel: FC = () => {
  const [tab, setTab] = useState<TabId>("workspace");
  const [collapsed, setCollapsed] = useState(false);

  if (collapsed) {
    return (
      <div className="flex w-12 flex-col items-center gap-1 border-l border-neutral-200 bg-white py-2">
        {TABS.map(({ id, label, icon: Icon }) => (
          <button
            key={id}
            type="button"
            title={label}
            onClick={() => {
              setTab(id);
              setCollapsed(false);
            }}
            className={`rounded-lg p-2 ${
              tab === id
                ? "bg-violet-100 text-violet-700"
                : "text-neutral-400 hover:bg-neutral-100 hover:text-neutral-600"
            }`}
          >
            <Icon size={18} />
          </button>
        ))}
      </div>
    );
  }

  return (
    <aside className="flex w-72 flex-col border-l border-neutral-200 bg-white">
      <div className="flex items-center gap-0.5 border-b border-neutral-200 px-2 py-2">
        {TABS.map(({ id, label, icon: Icon }) => (
          <button
            key={id}
            type="button"
            onClick={() => setTab(id)}
            title={label}
            className={`flex flex-1 flex-col items-center gap-1 rounded-lg px-1 py-1.5 text-[11px] font-medium transition-colors ${
              tab === id
                ? "bg-violet-100 text-violet-700"
                : "text-neutral-500 hover:bg-neutral-100 hover:text-neutral-700"
            }`}
          >
            <Icon size={16} />
            {label}
          </button>
        ))}
        <button
          type="button"
          title="Collapse panel"
          onClick={() => setCollapsed(true)}
          className="ml-0.5 rounded-lg p-1.5 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-600"
        >
          <ChevronRight size={16} />
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto">
        {tab === "workspace" && <WorkspaceTab />}
        {tab === "runs" && <RunsTab />}
        {tab === "memory" && <MemoryTab />}
        {tab === "skills" && <SkillsTab />}
      </div>
    </aside>
  );
};

/* ---------- Workspace ---------- */

type FileEntry = { path: string; op: "read" | "write" };

// Parse the file path out of a tool call's args (best effort — args may still be
// streaming, so tolerate partial JSON).
function pathOf(argsText: string): string | null {
  try {
    const v = JSON.parse(argsText) as { path?: unknown };
    return typeof v.path === "string" ? v.path : null;
  } catch {
    const m = /"path"\s*:\s*"([^"]*)"/.exec(argsText);
    return m ? m[1] : null;
  }
}

const WorkspaceTab: FC = () => {
  const { tools } = useActivity();
  const files = useMemo<FileEntry[]>(() => {
    const seen = new Map<string, FileEntry>();
    for (const t of tools) {
      if (t.toolName !== "read_file" && t.toolName !== "write_file") continue;
      const p = pathOf(t.argsText);
      if (!p) continue;
      // A later write upgrades the entry's op; reads don't downgrade a write.
      const op = t.toolName === "write_file" ? "write" : "read";
      const prev = seen.get(p);
      seen.set(p, { path: p, op: prev?.op === "write" ? "write" : op });
    }
    return [...seen.values()];
  }, [tools]);

  if (files.length === 0) {
    return (
      <EmptyState
        icon={FolderTree}
        title="No files yet"
        hint="Files the agent reads or writes in this conversation will appear here."
      />
    );
  }
  return (
    <ul className="p-2">
      {files.map((f) => (
        <li
          key={f.path}
          className="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-neutral-50"
        >
          <FileIcon path={f.path} />
          <span className="min-w-0 flex-1 truncate font-mono text-xs text-neutral-700">
            {f.path}
          </span>
          <span
            className={`rounded px-1.5 py-0.5 text-[10px] font-medium ${
              f.op === "write"
                ? "bg-violet-100 text-violet-700"
                : "bg-neutral-100 text-neutral-500"
            }`}
          >
            {f.op}
          </span>
        </li>
      ))}
    </ul>
  );
};

const FileIcon: FC<{ path: string }> = ({ path }) => {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  const Code =
    /^(py|js|ts|tsx|jsx|go|rs|java|c|cpp|sh|json|ya?ml|toml|html|css)$/.test(ext);
  const Icon = Code ? FileCode2 : ext === "md" || ext === "txt" ? FileText : File;
  return <Icon size={15} className="shrink-0 text-neutral-400" />;
};

/* ---------- Runs ---------- */

const RunsTab: FC = () => {
  const { tools } = useActivity();
  if (tools.length === 0) {
    return (
      <EmptyState
        icon={Activity}
        title="No runs yet"
        hint="Tool calls the agent makes will stream here as they run."
      />
    );
  }
  return (
    <ul className="p-2">
      {[...tools].reverse().map((t) => (
        <RunRow key={t.id} tool={t} />
      ))}
    </ul>
  );
};

const RunRow: FC<{ tool: ToolActivity }> = ({ tool }) => {
  const [open, setOpen] = useState(false);
  const resultText =
    tool.result === undefined || tool.result === null
      ? ""
      : typeof tool.result === "string"
        ? tool.result
        : JSON.stringify(tool.result, null, 2);
  return (
    <li className="mb-1 rounded-lg border border-neutral-200">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-2.5 py-2 text-left"
      >
        {tool.status === "running" ? (
          <Loader2 size={14} className="shrink-0 animate-spin text-violet-500" />
        ) : tool.status === "error" ? (
          <XCircle size={14} className="shrink-0 text-red-500" />
        ) : (
          <CheckCircle2 size={14} className="shrink-0 text-emerald-500" />
        )}
        <span className="min-w-0 flex-1 truncate font-mono text-xs font-medium text-neutral-800">
          {tool.toolName}
        </span>
        <span className="text-[10px] text-neutral-400">
          {new Date(tool.at).toLocaleTimeString([], {
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
          })}
        </span>
      </button>
      {open && (
        <div className="space-y-1.5 border-t border-neutral-100 px-2.5 py-2 font-mono text-[11px] leading-relaxed">
          {tool.argsText && (
            <pre className="whitespace-pre-wrap break-all text-neutral-500">
              {tool.argsText}
            </pre>
          )}
          {resultText && (
            <pre
              className={`whitespace-pre-wrap break-all ${
                tool.isError ? "text-red-600" : "text-neutral-600"
              }`}
            >
              {resultText}
            </pre>
          )}
        </div>
      )}
    </li>
  );
};

/* ---------- Memory / Skills (placeholders) ---------- */

const MemoryTab: FC = () => (
  <Placeholder
    icon={Brain}
    title="Memory"
    lines={[
      { k: "preference", v: "回复用中文" },
      { k: "pet", v: "养了一只叫豆豆的猫" },
    ]}
    hint="Long-term memory recall is on the roadmap — facts the agent remembers about you will be listed and editable here."
  />
);

const SkillsTab: FC = () => (
  <Placeholder
    icon={Blocks}
    title="Skills"
    lines={[
      { k: "file-tools", v: "read / write / list workspace files" },
      { k: "run-command", v: "execute shell in the sandbox (soon)" },
    ]}
    hint="Skill and script registration (SkillTool / ScriptTool) is planned — installed skills will be browsable and toggleable here."
  />
);

const Placeholder: FC<{
  icon: LucideIcon;
  title: string;
  lines: { k: string; v: string }[];
  hint: string;
}> = ({ icon: Icon, title, lines, hint }) => (
  <div className="p-3">
    <div className="mb-2 flex items-center gap-2 text-sm font-medium text-neutral-800">
      <Icon size={15} className="text-violet-500" />
      {title}
      <span className="rounded bg-amber-100 px-1.5 py-0.5 text-[10px] font-medium text-amber-700">
        preview
      </span>
    </div>
    <ul className="mb-3 space-y-1.5">
      {lines.map((l) => (
        <li
          key={l.k}
          className="rounded-lg border border-dashed border-neutral-200 bg-neutral-50 px-2.5 py-2"
        >
          <div className="text-[10px] font-medium uppercase tracking-wide text-neutral-400">
            {l.k}
          </div>
          <div className="text-xs text-neutral-700">{l.v}</div>
        </li>
      ))}
    </ul>
    <p className="text-[11px] leading-relaxed text-neutral-400">{hint}</p>
  </div>
);

const EmptyState: FC<{ icon: LucideIcon; title: string; hint: string }> = ({
  icon: Icon,
  title,
  hint,
}) => (
  <div className="flex h-full flex-col items-center justify-center gap-2 p-6 text-center">
    <Icon size={22} className="text-neutral-300" />
    <div className="text-sm font-medium text-neutral-500">{title}</div>
    <div className="text-[11px] leading-relaxed text-neutral-400">{hint}</div>
  </div>
);
