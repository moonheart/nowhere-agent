// SkillEditor is the three-pane skill editing surface (skill-console): a skill
// list on the left, the selected skill's files (SKILL.md + resources + scripts)
// in the middle, and a CodeMirror editor on the right. Saving writes a new
// version; the version dropdown views history and can roll back.
//
// It is scope-agnostic: the `base` prop says whose skills these are (the signed
// in user, one team, or the platform), and every call goes through lib/skills
// with that base. Read-only when `canWrite` is false (a plain team member).

import { useEffect, useMemo, useState } from "react";
import CodeMirror from "@uiw/react-codemirror";
import { markdown } from "@codemirror/lang-markdown";
import { python } from "@codemirror/lang-python";
import { javascript } from "@codemirror/lang-javascript";
import type { Extension } from "@codemirror/state";
import {
  FileCode2,
  FileText,
  FolderInput,
  History,
  Loader2,
  Plus,
  Save,
  Sparkles,
  Trash2,
} from "lucide-react";
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ConfirmButton } from "@/components/admin/confirm";
import { ErrorNotice } from "@/components/admin/common";
import { cn } from "@/lib/utils";
import { useMe, canManageTeam } from "@/lib/me";
import {
  createSkill,
  deleteSkill,
  disableSkill,
  enableSkill,
  getSkill,
  listSkills,
  moveSkillToTeam,
  rollbackSkill,
  skillVersionAt,
  skillVersions,
  updateSkill,
  type Skill,
  type SkillBase,
  type SkillVersion,
} from "@/lib/skills";

// One editable file inside a skill: "body" is SKILL.md; resources and scripts
// are named L2 files.
type SkillFile =
  | { kind: "body" }
  | { kind: "resource"; path: string }
  | { kind: "script"; path: string };

function fileLabel(f: SkillFile): string {
  return f.kind === "body" ? "SKILL.md" : f.path;
}

function fileKey(f: SkillFile): string {
  return f.kind === "body" ? "body" : `${f.kind}:${f.path}`;
}

// languageFor picks the CodeMirror language extension from the file name.
function languageFor(name: string): Extension[] {
  const ext = name.slice(name.lastIndexOf(".")).toLowerCase();
  if (ext === ".py") return [python()];
  if (ext === ".js" || ext === ".mjs") return [javascript()];
  return [markdown()];
}

// Draft is the editable form of a skill (the whole skill, edited file by file).
type Draft = {
  name: string;
  description: string;
  body: string;
  resources: Record<string, string>;
  scripts: Record<string, string>;
};

function draftOf(sk: Skill): Draft {
  return {
    name: sk.name,
    description: sk.description,
    body: sk.body,
    resources: { ...sk.resources },
    scripts: { ...sk.scripts },
  };
}

export function SkillEditor({ base, canWrite }: { base: SkillBase; canWrite: boolean }) {
  const [skills, setSkills] = useState<Skill[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [current, setCurrent] = useState<Skill | null>(null);
  const [file, setFile] = useState<SkillFile>({ kind: "body" });
  const [versions, setVersions] = useState<SkillVersion[]>([]);
  const [viewVersion, setViewVersion] = useState<number | null>(null); // null = current
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const [moveOpen, setMoveOpen] = useState(false);
  const [moveTeamId, setMoveTeamId] = useState<string>("");
  const { me } = useMe();

  const viewingHistory = viewVersion !== null && current !== null && viewVersion !== current.current_version;

  const refreshList = async (keepSelected?: string) => {
    try {
      const { skills } = await listSkills(base);
      setSkills(skills);
      setListError(null);
      if (keepSelected) setSelectedId(keepSelected);
    } catch (e) {
      setListError((e as Error).message);
    }
  };

  useEffect(() => {
    void refreshList();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [base.kind, base.kind === "team" ? base.teamId : ""]);

  // Load the selected skill's current content.
  useEffect(() => {
    if (!selectedId) {
      setCurrent(null);
      setDraft(null);
      setVersions([]);
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const [{ skill }, { versions }] = await Promise.all([
          getSkill(base, selectedId),
          skillVersions(base, selectedId),
        ]);
        if (cancelled) return;
        setCurrent(skill);
        setDraft(draftOf(skill));
        setVersions(versions);
        setViewVersion(null);
        setFile({ kind: "body" });
        setDirty(false);
        setError(null);
      } catch (e) {
        if (!cancelled) setError((e as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedId]);

  const openVersion = async (v: number) => {
    if (!selectedId || !current) return;
    if (!confirmDiscard()) return;
    if (v === current.current_version) {
      setViewVersion(null);
      setDraft(draftOf(current));
      setDirty(false);
      return;
    }
    try {
      const { skill } = await skillVersionAt(base, selectedId, v);
      setViewVersion(v);
      setDraft(draftOf(skill)); // read-only view of the historical revision
      setDirty(false);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const fileContent = useMemo(() => {
    if (!draft) return "";
    if (file.kind === "body") return draft.body;
    return file.kind === "resource" ? draft.resources[file.path] ?? "" : draft.scripts[file.path] ?? "";
  }, [draft, file]);

  const setFileContent = (value: string) => {
    if (!draft || viewingHistory || !canWrite) return;
    const next = { ...draft };
    if (file.kind === "body") next.body = value;
    else if (file.kind === "resource") next.resources = { ...draft.resources, [file.path]: value };
    else next.scripts = { ...draft.scripts, [file.path]: value };
    setDraft(next);
    setDirty(true);
  };

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    setError(null);
    try {
      const body = {
        name: draft.name,
        description: draft.description,
        body: draft.body,
        resources: draft.resources,
        scripts: draft.scripts,
      };
      const { skill } = selectedId
        ? await updateSkill(base, selectedId, body)
        : await createSkill(base, body);
      await refreshList(skill.id);
      setCurrent(skill);
      setDraft(draftOf(skill));
      setViewVersion(null);
      setDirty(false);
      const { versions } = await skillVersions(base, skill.id);
      setVersions(versions);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const startNew = () => {
    if (!confirmDiscard()) return;
    setSelectedId(null);
    setCurrent(null);
    setDraft({ name: "", description: "", body: "", resources: {}, scripts: {} });
    setFile({ kind: "body" });
    setVersions([]);
    setViewVersion(null);
    setDirty(false);
    setError(null);
  };

  const remove = async () => {
    if (!selectedId) return;
    await deleteSkill(base, selectedId);
    setSelectedId(null);
    setCurrent(null);
    setDraft(null);
    await refreshList();
  };

  // confirmDiscard prompts before a navigation that would drop unsaved edits:
  // switching skills, starting a new skill, or opening a historical version.
  const confirmDiscard = (): boolean => {
    if (!dirty) return true;
    return window.confirm("You have unsaved changes. Discard them?");
  };

  // Toggle the agent-resolution gate without a version bump. The backend returns
  // the updated skill; sync it into `current` and the list.
  const setEnabled = async (enabled: boolean) => {
    if (!selectedId || !current) return;
    setBusy(true);
    setError(null);
    try {
      const { skill } = enabled
        ? await enableSkill(base, selectedId)
        : await disableSkill(base, selectedId);
      setCurrent(skill);
      setSkills((prev) => prev?.map((s) => (s.id === skill.id ? skill : s)) ?? prev);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // Teams the signed-in account can push a skill into (write access). Move is
  // self-scope only, so this is only consulted for the "me" editor.
  const movableTeams = (me?.teams ?? []).filter((t) => canManageTeam(me, t.id));

  const moveToTeam = async () => {
    if (!selectedId || !moveTeamId) return;
    // Moving drops the selection and the draft — guard unsaved edits the same
    // way the skill-switch and startNew paths do.
    if (!confirmDiscard()) return;
    setBusy(true);
    setError(null);
    try {
      await moveSkillToTeam(selectedId, moveTeamId);
      // The skill left the user scope: drop the selection and refresh the list.
      setMoveOpen(false);
      setSelectedId(null);
      setCurrent(null);
      setDraft(null);
      setDirty(false);
      await refreshList();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const rollback = async (v: number) => {
    if (!selectedId) return;
    setBusy(true);
    setError(null);
    try {
      const { skill } = await rollbackSkill(base, selectedId, v);
      await refreshList(skill.id);
      setCurrent(skill);
      setDraft(draftOf(skill));
      setViewVersion(null);
      setDirty(false);
      const { versions } = await skillVersions(base, skill.id);
      setVersions(versions);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const addFile = (kind: "resource" | "script", path: string) => {
    if (!draft) return;
    const p = path.trim();
    if (!p) return;
    if (kind === "resource") {
      if (draft.resources[p] !== undefined) return;
      setDraft({ ...draft, resources: { ...draft.resources, [p]: "" } });
    } else {
      if (draft.scripts[p] !== undefined) return;
      setDraft({ ...draft, scripts: { ...draft.scripts, [p]: "" } });
    }
    setFile({ kind, path: p });
    setDirty(true);
  };

  const removeFile = (f: SkillFile) => {
    if (!draft || f.kind === "body") return;
    const next = { ...draft };
    if (f.kind === "resource") {
      next.resources = { ...draft.resources };
      delete next.resources[f.path];
    } else {
      next.scripts = { ...draft.scripts };
      delete next.scripts[f.path];
    }
    setDraft(next);
    setFile({ kind: "body" });
    setDirty(true);
  };

  if (listError) return <ErrorNotice message={listError} />;

  return (
    <ResizablePanelGroup className="min-h-0 w-full flex-1 rounded-lg border border-border">
      {/* ---- skill list ---- */}
      <ResizablePanel defaultSize={22} minSize={14}>
        <div className="flex h-full flex-col">
          <div className="flex items-center justify-between px-3 py-2">
            <span className="text-xs font-medium tracking-wide text-muted-foreground uppercase">
              Skills
            </span>
            {canWrite && (
              <Button variant="ghost" size="sm" onClick={startNew} title="New skill">
                <Plus className="size-4" />
              </Button>
            )}
          </div>
          <Separator />
          <ScrollArea className="min-h-0 flex-1">
            {skills === null ? (
              <div className="flex items-center gap-2 p-3 text-sm text-muted-foreground">
                <Loader2 className="size-4 animate-spin" /> Loading…
              </div>
            ) : skills.length === 0 ? (
              <div className="p-3 text-sm text-muted-foreground">
                No skills yet.{canWrite ? " Create one to get started." : ""}
              </div>
            ) : (
              skills.map((sk) => (
                <button
                  key={sk.id}
                  onClick={() => {
                    if (confirmDiscard()) setSelectedId(sk.id);
                  }}
                  className={cn(
                    "flex w-full items-center gap-2 px-3 py-2 text-left text-sm transition-colors",
                    selectedId === sk.id
                      ? "bg-muted font-medium text-foreground"
                      : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
                  )}
                >
                  <Sparkles className="size-3.5 shrink-0" />
                  <span className="min-w-0 flex-1 truncate">{sk.name}</span>
                  {!sk.enabled && (
                    <Badge variant="outline" className="shrink-0 text-[10px] text-muted-foreground">
                      off
                    </Badge>
                  )}
                  {sk.needs_review && (
                    <Badge variant="secondary" className="shrink-0 text-[10px]">
                      review
                    </Badge>
                  )}
                  <span className="shrink-0 text-[10px] text-muted-foreground">v{sk.current_version}</span>
                </button>
              ))
            )}
          </ScrollArea>
        </div>
      </ResizablePanel>

      <ResizableHandle withHandle />

      {/* ---- file tree ---- */}
      <ResizablePanel defaultSize={20} minSize={14}>
        <FileTree
          draft={draft}
          file={file}
          onSelect={setFile}
          onAdd={addFile}
          onRemove={removeFile}
          readOnly={!canWrite || viewingHistory}
        />
      </ResizablePanel>

      <ResizableHandle withHandle />

      {/* ---- editor ---- */}
      <ResizablePanel defaultSize={58} minSize={30}>
        <div className="flex h-full flex-col">
          {/* toolbar */}
          <div className="flex flex-wrap items-center gap-2 px-3 py-2">
            {draft && selectedId && current ? (
              <>
                <History className="size-4 text-muted-foreground" />
                <Select
                  value={String(viewVersion ?? current.current_version)}
                  onValueChange={(v) => void openVersion(Number(v))}
                >
                  <SelectTrigger className="h-8 w-32">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {versions.map((v) => (
                      <SelectItem key={v.version} value={String(v.version)}>
                        v{v.version}
                        {v.version === current.current_version ? " (current)" : ""}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {viewingHistory && canWrite && (
                  <ConfirmButton
                    title={`Roll back to v${viewVersion}?`}
                    description="This saves the historical version's content as a new current version. History is kept."
                    confirmLabel="Roll back"
                    onConfirm={() => rollback(viewVersion!)}
                    trigger={
                      <Button variant="outline" size="sm">
                        Roll back
                      </Button>
                    }
                  />
                )}
              </>
            ) : null}
            <div className="ml-auto flex items-center gap-2">
              {selectedId && current && canWrite && (
                <div className="flex items-center gap-1.5" title="Disabled skills are hidden from the agent but stay editable here">
                  <Switch
                    id="skill-enabled"
                    checked={current.enabled}
                    disabled={busy}
                    onCheckedChange={(v) => void setEnabled(v)}
                  />
                  <Label htmlFor="skill-enabled" className="text-xs text-muted-foreground">
                    {current.enabled ? "Enabled" : "Disabled"}
                  </Label>
                </div>
              )}
              {selectedId && canWrite && base.kind === "me" && movableTeams.length > 0 && (
                <Button
                  variant="ghost"
                  size="sm"
                  title="Move to a team"
                  onClick={() => {
                    setMoveTeamId(movableTeams[0]?.id ?? "");
                    setMoveOpen(true);
                  }}
                >
                  <FolderInput className="size-4" />
                </Button>
              )}
              {selectedId && canWrite && (
                <ConfirmButton
                  title="Delete this skill?"
                  description="The skill and its whole version history are removed. This cannot be undone."
                  confirmLabel="Delete"
                  onConfirm={remove}
                  trigger={
                    <Button variant="ghost" size="sm" title="Delete skill">
                      <Trash2 className="size-4" />
                    </Button>
                  }
                />
              )}
              {canWrite && (
                <Button size="sm" onClick={() => void save()} disabled={busy || !draft || (!dirty && !!selectedId)}>
                  {busy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                  Save
                </Button>
              )}
            </div>
          </div>
          <Separator />
          {error && <div className="px-3 pt-2"><ErrorNotice message={error} /></div>}

          {draft === null ? (
            <div className="flex flex-1 items-center justify-center p-6 text-sm text-muted-foreground">
              Select a skill{canWrite ? ", or create a new one" : ""} to edit.
            </div>
          ) : (
            <div className="flex min-h-0 flex-1 flex-col">
              {/* name + description */}
              <div className="space-y-2 px-3 py-2">
                <Input
                  value={draft.name}
                  onChange={(e) => {
                    setDraft({ ...draft, name: e.target.value });
                    setDirty(true);
                  }}
                  placeholder="skill name"
                  disabled={!canWrite || viewingHistory || !!selectedId}
                  className="h-8 font-medium"
                  title={selectedId ? "Name is fixed after creation (it identifies the skill)" : "skill name"}
                />
                <Input
                  value={draft.description}
                  onChange={(e) => {
                    setDraft({ ...draft, description: e.target.value });
                    setDirty(true);
                  }}
                  placeholder="one-line description (shown in the skill index)"
                  disabled={!canWrite || viewingHistory}
                  className="h-8 text-sm"
                />
              </div>
              <Separator />
              <div className="flex items-center gap-2 px-3 py-1.5 text-xs text-muted-foreground">
                {file.kind === "body" ? <FileText className="size-3.5" /> : <FileCode2 className="size-3.5" />}
                <span className="font-mono">{fileLabel(file)}</span>
                {viewingHistory && <Badge variant="outline" className="text-[10px]">read-only history</Badge>}
              </div>
              <div className="min-h-0 flex-1 overflow-auto">
                <CodeMirror
                  value={fileContent}
                  height="100%"
                  theme="dark"
                  extensions={languageFor(fileLabel(file))}
                  editable={canWrite && !viewingHistory}
                  onChange={setFileContent}
                  basicSetup={{ lineNumbers: true, foldGutter: true }}
                />
              </div>
            </div>
          )}
        </div>
      </ResizablePanel>

      {/* ---- move-to-team dialog (self scope only) ---- */}
      <Dialog open={moveOpen} onOpenChange={setMoveOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Move “{current?.name}” to a team</DialogTitle>
            <DialogDescription>
              The skill and its whole version history move into the team's shared
              scope, where every member can use it. It leaves your personal
              skills. Only teams you can manage are listed.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="move-team" className="text-sm">
              Destination team
            </Label>
            <Select value={moveTeamId} onValueChange={(v) => setMoveTeamId(v ?? "")}>
              <SelectTrigger id="move-team">
                <SelectValue placeholder="Select a team" />
              </SelectTrigger>
              <SelectContent>
                {movableTeams.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setMoveOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => void moveToTeam()} disabled={busy || !moveTeamId}>
              {busy ? <Loader2 className="size-4 animate-spin" /> : <FolderInput className="size-4" />}
              Move to team
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </ResizablePanelGroup>
  );
}

// FileTree lists SKILL.md + resources + scripts, with add/remove for L2 files.
function FileTree({
  draft,
  file,
  onSelect,
  onAdd,
  onRemove,
  readOnly,
}: {
  draft: Draft | null;
  file: SkillFile;
  onSelect: (f: SkillFile) => void;
  onAdd: (kind: "resource" | "script", path: string) => void;
  onRemove: (f: SkillFile) => void;
  readOnly: boolean;
}) {
  const [newPath, setNewPath] = useState("");
  const [newKind, setNewKind] = useState<"resource" | "script">("script");

  if (!draft) {
    return <div className="p-3 text-sm text-muted-foreground">No skill selected.</div>;
  }

  const item = (f: SkillFile, icon: React.ReactNode, removable: boolean) => (
    <div
      key={fileKey(f)}
      className={cn(
        "group flex items-center gap-2 px-3 py-1.5 text-sm",
        fileKey(file) === fileKey(f)
          ? "bg-muted font-medium text-foreground"
          : "text-muted-foreground hover:bg-muted/60",
      )}
    >
      <button onClick={() => onSelect(f)} className="flex min-w-0 flex-1 items-center gap-2 text-left">
        {icon}
        <span className="truncate font-mono text-xs">{fileLabel(f)}</span>
      </button>
      {removable && !readOnly && (
        <button
          onClick={() => onRemove(f)}
          className="hidden shrink-0 text-muted-foreground hover:text-destructive group-hover:block"
          title="Remove file"
        >
          <Trash2 className="size-3.5" />
        </button>
      )}
    </div>
  );

  return (
    <div className="flex h-full flex-col">
      <div className="px-3 py-2 text-xs font-medium tracking-wide text-muted-foreground uppercase">
        Files
      </div>
      <Separator />
      <ScrollArea className="min-h-0 flex-1">
        {item({ kind: "body" }, <FileText className="size-3.5 shrink-0" />, false)}
        {Object.keys(draft.scripts).sort().map((p) =>
          item({ kind: "script", path: p }, <FileCode2 className="size-3.5 shrink-0 text-primary" />, true),
        )}
        {Object.keys(draft.resources).sort().map((p) =>
          item({ kind: "resource", path: p }, <FileText className="size-3.5 shrink-0" />, true),
        )}
      </ScrollArea>
      {!readOnly && (
        <>
          <Separator />
          <div className="space-y-1.5 p-2">
            <div className="flex gap-1">
              <Select value={newKind} onValueChange={(v) => setNewKind(v as "resource" | "script")}>
                <SelectTrigger className="h-7 w-24 text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="script">script</SelectItem>
                  <SelectItem value="resource">resource</SelectItem>
                </SelectContent>
              </Select>
              <Input
                value={newPath}
                onChange={(e) => setNewPath(e.target.value)}
                placeholder={newKind === "script" ? "run.py" : "notes.md"}
                className="h-7 font-mono text-xs"
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    onAdd(newKind, newPath);
                    setNewPath("");
                  }
                }}
              />
            </div>
            <Button
              variant="outline"
              size="sm"
              className="h-7 w-full text-xs"
              onClick={() => {
                onAdd(newKind, newPath);
                setNewPath("");
              }}
            >
              <Plus className="size-3.5" /> Add file
            </Button>
          </div>
        </>
      )}
    </div>
  );
}
