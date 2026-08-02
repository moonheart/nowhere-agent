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
  Bot,
  type LucideIcon,
} from "lucide-react";
import { useActivity, type ToolActivity, type SubagentRun } from "@/lib/activity";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";

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
      <div className="flex w-12 flex-col items-center gap-1 border-l border-border py-2">
        {TABS.map(({ id, label, icon: Icon }) => (
          <Button
            key={id}
            variant="ghost"
            size="icon"
            title={label}
            aria-label={label}
            onClick={() => {
              setTab(id);
              setCollapsed(false);
            }}
            className={cn(
              tab === id
                ? "bg-primary/10 text-primary hover:bg-primary/15 hover:text-primary"
                : "text-muted-foreground",
            )}
          >
            <Icon />
          </Button>
        ))}
      </div>
    );
  }

  return (
    <aside className="flex w-72 flex-col border-l border-border bg-background">
      <Tabs
        value={tab}
        onValueChange={(v) => setTab(v as TabId)}
        className="min-h-0 flex-1 gap-0"
      >
        <div className="flex items-center gap-0.5 border-b border-border px-2 py-2">
          <TabsList variant="line" className="h-auto flex-1">
            {TABS.map(({ id, label, icon: Icon }) => (
              <TabsTrigger
                key={id}
                value={id}
                title={label}
                className="h-auto flex-col gap-1 py-1.5 text-[11px]"
              >
                <Icon />
                {label}
              </TabsTrigger>
            ))}
          </TabsList>
          <Button
            variant="ghost"
            size="icon-sm"
            title="Collapse panel"
            aria-label="Collapse panel"
            onClick={() => setCollapsed(true)}
            className="text-muted-foreground"
          >
            <ChevronRight />
          </Button>
        </div>
        <ScrollArea className="min-h-0 flex-1">
          <TabsContent value="workspace">
            <WorkspaceTab />
          </TabsContent>
          <TabsContent value="runs">
            <RunsTab />
          </TabsContent>
          <TabsContent value="memory">
            <MemoryTab />
          </TabsContent>
          <TabsContent value="skills">
            <SkillsTab />
          </TabsContent>
        </ScrollArea>
      </Tabs>
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
      <TabEmpty
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
          className="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-muted/60"
        >
          <FileIcon path={f.path} />
          <span className="min-w-0 flex-1 truncate font-mono text-xs text-foreground/80">
            {f.path}
          </span>
          <Badge variant={f.op === "write" ? "default" : "secondary"}>
            {f.op}
          </Badge>
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
  return <Icon className="size-4 shrink-0 text-muted-foreground" />;
};

/* ---------- Runs ---------- */

const RunsTab: FC = () => {
  const { tools, subagents } = useActivity();
  if (tools.length === 0 && subagents.length === 0) {
    return (
      <TabEmpty
        icon={Activity}
        title="No runs yet"
        hint="Tool calls the agent makes will stream here as they run."
      />
    );
  }
  return (
    <div className="p-2">
      {subagents.length > 0 && (
        <div className="mb-2">
          <div className="px-1 pb-1 text-[10px] font-semibold tracking-wide text-muted-foreground uppercase">
            Subagents
          </div>
          <ul>
            {[...subagents].reverse().map((s) => (
              <SubagentRow key={s.id} run={s} />
            ))}
          </ul>
        </div>
      )}
      {tools.length > 0 && (
        <ul>
          {[...tools].reverse().map((t) => (
            <RunRow key={t.id} tool={t} />
          ))}
        </ul>
      )}
    </div>
  );
};

// StatusIcon is the running / error / done glyph shared by both row kinds.
const StatusIcon: FC<{ status: string }> = ({ status }) =>
  status === "running" ? (
    <Loader2 className="size-3.5 shrink-0 animate-spin text-primary" />
  ) : status === "error" ? (
    <XCircle className="size-3.5 shrink-0 text-destructive" />
  ) : (
    <CheckCircle2 className="size-3.5 shrink-0 text-emerald-500" />
  );

const SubagentRow: FC<{ run: SubagentRun }> = ({ run }) => (
  <li className="mb-1 rounded-lg border border-primary/20 bg-primary/5 px-2.5 py-2">
    <div className="flex items-center gap-2">
      <StatusIcon status={run.status} />
      <Bot className="size-3.5 shrink-0 text-primary" />
      <span className="min-w-0 flex-1 truncate text-xs font-medium text-foreground">
        {run.agentType}
      </span>
      {run.depth > 1 && (
        <Badge variant="secondary" className="h-4 px-1 text-[10px]">
          L{run.depth}
        </Badge>
      )}
    </div>
    {run.tools.length > 0 && (
      <div className="mt-1 pl-6 font-mono text-[10px] text-muted-foreground">
        {run.tools.join(" · ")}
      </div>
    )}
  </li>
);

const RunRow: FC<{ tool: ToolActivity }> = ({ tool }) => {
  const [open, setOpen] = useState(false);
  const resultText =
    tool.result === undefined || tool.result === null
      ? ""
      : typeof tool.result === "string"
        ? tool.result
        : JSON.stringify(tool.result, null, 2);
  return (
    <li className="mb-1">
      <Collapsible
        open={open}
        onOpenChange={setOpen}
        className="rounded-lg border border-border"
      >
        <CollapsibleTrigger className="flex w-full items-center gap-2 px-2.5 py-2 text-left">
          <StatusIcon status={tool.status} />
          <span className="min-w-0 flex-1 truncate font-mono text-xs font-medium text-foreground">
            {tool.toolName}
          </span>
          <span className="text-[10px] text-muted-foreground">
            {new Date(tool.at).toLocaleTimeString([], {
              hour: "2-digit",
              minute: "2-digit",
              second: "2-digit",
            })}
          </span>
        </CollapsibleTrigger>
        <CollapsibleContent className="space-y-1.5 border-t border-border px-2.5 py-2 font-mono text-[11px] leading-relaxed">
          {tool.argsText && (
            <pre className="break-all whitespace-pre-wrap text-muted-foreground">
              {tool.argsText}
            </pre>
          )}
          {resultText && (
            <pre
              className={cn(
                "break-all whitespace-pre-wrap",
                tool.isError ? "text-destructive" : "text-foreground/70",
              )}
            >
              {resultText}
            </pre>
          )}
        </CollapsibleContent>
      </Collapsible>
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
    <div className="mb-2 flex items-center gap-2 text-sm font-medium text-foreground">
      <Icon className="size-4 text-primary" />
      {title}
      <Badge
        variant="outline"
        className="border-amber-500/40 text-amber-700 dark:text-amber-400"
      >
        preview
      </Badge>
    </div>
    <ul className="mb-3 space-y-1.5">
      {lines.map((l) => (
        <li
          key={l.k}
          className="rounded-lg border border-dashed border-border bg-muted/50 px-2.5 py-2"
        >
          <div className="text-[10px] font-medium tracking-wide text-muted-foreground uppercase">
            {l.k}
          </div>
          <div className="text-xs text-foreground/80">{l.v}</div>
        </li>
      ))}
    </ul>
    <p className="text-[11px] leading-relaxed text-muted-foreground">{hint}</p>
  </div>
);

const TabEmpty: FC<{ icon: LucideIcon; title: string; hint: string }> = ({
  icon: Icon,
  title,
  hint,
}) => (
  <Empty className="p-6">
    <EmptyHeader>
      <EmptyMedia variant="icon">
        <Icon />
      </EmptyMedia>
      <EmptyTitle>{title}</EmptyTitle>
      <EmptyDescription className="text-[11px] leading-relaxed">
        {hint}
      </EmptyDescription>
    </EmptyHeader>
  </Empty>
);
