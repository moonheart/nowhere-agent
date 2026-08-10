import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type FC,
  type PointerEvent as ReactPointerEvent,
} from "react";
import {
  FolderTree,
  Blocks,
  File,
  FileCode2,
  FileText,
  ChevronRight,
  type LucideIcon,
} from "lucide-react";
import { useActivity } from "@/lib/activity";
import { listSkills, enableSkill, disableSkill, type Skill } from "@/lib/skills";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";

type TabId = "workspace" | "skills";

const TABS: { id: TabId; label: string; icon: LucideIcon }[] = [
  { id: "workspace", label: "Workspace", icon: FolderTree },
  { id: "skills", label: "Skills", icon: Blocks },
];

/**
 * Right-hand inspector panel. Two tabs: Workspace (files the agent touched in
 * this conversation) and Skills (the caller's user-scope skills, toggleable).
 * Workspace data comes from the in-app activity feed the thread publishes;
 * Skills loads from the self-service API on tab activation. Long-term memory
 * management lives in the Console (My page), not here.
 */
// Panel width bounds (px) for the drag resize. The default sits between them;
// the user's last drag is remembered in localStorage.
const PANEL_MIN_WIDTH = 320;
const PANEL_MAX_WIDTH = 720;
const PANEL_DEFAULT_WIDTH = 448;
const PANEL_WIDTH_KEY = "right-panel-width";

function storedPanelWidth(): number {
  try {
    const v = Number(localStorage.getItem(PANEL_WIDTH_KEY));
    if (Number.isFinite(v)) {
      return Math.min(PANEL_MAX_WIDTH, Math.max(PANEL_MIN_WIDTH, v));
    }
  } catch {
    // private mode etc. — fall through to the default
  }
  return PANEL_DEFAULT_WIDTH;
}

export const RightPanel: FC = () => {
  const [tab, setTab] = useState<TabId>("workspace");
  const [collapsed, setCollapsed] = useState(false);
  const [width, setWidth] = useState(storedPanelWidth);
  // The drag's anchor: the pointer's start x and the panel width at grab time.
  const dragRef = useRef<{ startX: number; startW: number } | null>(null);

  // Pointer-capture drag on the left edge: dragging left widens the panel
  // (it sits on the right of the screen). Text selection is suppressed for
  // the duration so a fast drag doesn't select the content underneath.
  const onHandlePointerDown = (e: ReactPointerEvent<HTMLDivElement>) => {
    dragRef.current = { startX: e.clientX, startW: width };
    e.currentTarget.setPointerCapture(e.pointerId);
    document.body.style.userSelect = "none";
  };
  const onHandlePointerMove = (e: ReactPointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    if (!drag) return;
    const w = Math.min(
      PANEL_MAX_WIDTH,
      Math.max(PANEL_MIN_WIDTH, drag.startW + (drag.startX - e.clientX)),
    );
    setWidth(w);
  };
  const endDrag = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (!dragRef.current) return;
    dragRef.current = null;
    document.body.style.userSelect = "";
    if (e.currentTarget.hasPointerCapture(e.pointerId)) {
      e.currentTarget.releasePointerCapture(e.pointerId);
    }
    try {
      localStorage.setItem(PANEL_WIDTH_KEY, String(width));
    } catch {
      // private mode etc. — the width just isn't remembered
    }
  };

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
    <aside
      className="relative flex shrink-0 flex-col border-l border-border bg-background"
      style={{ width }}
    >
      {/* Drag handle: the panel's left edge. */}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize panel"
        onPointerDown={onHandlePointerDown}
        onPointerMove={onHandlePointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        className="absolute top-0 bottom-0 left-0 z-10 w-1.5 cursor-col-resize touch-none select-none bg-transparent transition-colors hover:bg-border/70 active:bg-border"
      />
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
                className="h-auto py-1.5 pr-2 text-[11px]"
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
          <TabsContent value="skills">
            {/* Tabs keep inactive content mounted, so the tab passes its
                activation down to trigger a refresh. */}
            <SkillsTab active={tab === "skills"} />
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

/* ---------- Skills ---------- */

const SkillsTab: FC<{ active: boolean }> = ({ active }) => {
  const [skills, setSkills] = useState<Skill[] | null>(null);
  const [error, setError] = useState("");
  // id of the skill whose toggle request is in flight.
  const [busy, setBusy] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const { skills } = await listSkills({ kind: "me" });
      setSkills(skills.sort((a, b) => a.name.localeCompare(b.name)));
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    if (active) void load();
  }, [active, load]);

  const toggle = async (s: Skill) => {
    if (busy) return;
    setBusy(s.id);
    try {
      await (s.enabled ? disableSkill({ kind: "me" }, s.id) : enableSkill({ kind: "me" }, s.id));
      setSkills((prev) =>
        prev ? prev.map((x) => (x.id === s.id ? { ...x, enabled: !s.enabled } : x)) : prev,
      );
    } catch {
      // keep the previous state on failure
    } finally {
      setBusy(null);
    }
  };

  if (skills === null && !error) {
    return <TabLoading label="Loading skills…" />;
  }
  if (skills !== null && skills.length === 0) {
    return (
      <TabEmpty
        icon={Blocks}
        title="No skills yet"
        hint="Skills you author in the Console appear here. Team and system skills are managed there too."
      />
    );
  }
  return (
    <div className="p-2">
      {error && (
        <p className="px-1 pb-2 text-[11px] text-destructive">Failed to load skills: {error}</p>
      )}
      <ul className="space-y-1">
        {skills?.map((s) => (
          <li
            key={s.id}
            className={cn(
              "flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-2.5 py-2",
              !s.enabled && "opacity-60",
            )}
          >
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-1.5">
                <span className="truncate text-xs font-medium text-foreground">
                  {s.name}
                </span>
                <Badge variant="secondary" className="h-4 shrink-0 px-1 text-[10px]">
                  v{s.current_version}
                </Badge>
              </div>
              {s.description && (
                <p className="mt-0.5 truncate text-[11px] text-muted-foreground">
                  {s.description}
                </p>
              )}
            </div>
            <Switch
              size="sm"
              checked={s.enabled}
              disabled={busy === s.id}
              onCheckedChange={() => void toggle(s)}
              aria-label={`${s.enabled ? "Disable" : "Enable"} ${s.name}`}
            />
          </li>
        ))}
      </ul>
    </div>
  );
};

/* ---------- shared ---------- */

const TabLoading: FC<{ label: string }> = ({ label }) => (
  <div className="p-4 text-center text-xs text-muted-foreground">{label}</div>
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
