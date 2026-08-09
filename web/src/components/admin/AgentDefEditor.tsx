// AgentDefEditor is the agent-definition editing surface (persist-agent-defs):
// a definition list on the left, a markdown editor on the right. One instance
// serves one scope tier (self / team / platform) via the `base` prop; writes
// go to that base only. Read-only when `canWrite` is false (a plain team
// member).

import { useState } from "react";
import { Bot, Loader2, Plus, Trash2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  createAgentDef,
  deleteAgentDef,
  listAgentDefs,
  updateAgentDef,
  type AgentDef,
  type AgentDefBase,
} from "@/lib/agentdefs";
import { ErrorNotice, useAsync } from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";
import { cn } from "@/lib/utils";

const TEMPLATE = `---
name: my-agent
description: When the parent agent should pick this agent
tools: read_file, run_command
# disallowedTools: write_file
# model:
# maxTurns: 25
# skills: lint
---

You are a specialized subagent. You do not see the parent conversation —
work only from the task prompt. Finish with one self-contained message:
it is the only thing returned to the agent that launched you.
`;

export function AgentDefEditor({ base, canWrite }: { base: AgentDefBase; canWrite: boolean }) {
  const { data, loading, error, reload } = useAsync(() => listAgentDefs(base), [base.kind]);
  const defs = data?.defs ?? [];

  const [selected, setSelected] = useState<string | null>(null); // name, or "new"
  const [draft, setDraft] = useState("");
  const [originalName, setOriginalName] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [warnings, setWarnings] = useState<string[]>([]);

  const open = (d: AgentDef) => {
    setSelected(d.name);
    setOriginalName(d.name);
    setDraft(d.document);
    setSaveError(null);
    setWarnings([]);
  };

  const openNew = () => {
    setSelected("new");
    setOriginalName(null);
    setDraft(TEMPLATE);
    setSaveError(null);
    setWarnings([]);
  };

  const save = async () => {
    setBusy(true);
    setSaveError(null);
    try {
      const res =
        originalName === null
          ? await createAgentDef(base, draft)
          : await updateAgentDef(base, originalName, draft);
      setWarnings(res.warnings ?? []);
      setOriginalName(res.def.name);
      setSelected(res.def.name);
      reload();
    } catch (e) {
      // Validation errors stay in place so the draft is not lost.
      setSaveError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (name: string) => {
    await deleteAgentDef(base, name);
    if (selected === name) {
      setSelected(null);
      setDraft("");
      setOriginalName(null);
    }
    reload();
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center py-16 text-sm text-muted-foreground">
        <Loader2 className="mr-2 size-4 animate-spin" />
        Loading agent definitions…
      </div>
    );
  }
  if (error) return <ErrorNotice message={error} />;

  const current = defs.find((d) => d.name === originalName) ?? null;

  return (
    <div className="grid gap-4 lg:grid-cols-[260px_1fr]">
      <div className="space-y-1">
        {canWrite && (
          <Button size="sm" variant="outline" className="mb-2 w-full" onClick={openNew}>
            <Plus className="size-3.5" />
            New agent
          </Button>
        )}
        {defs.length === 0 && (
          <p className="px-1 py-4 text-sm text-muted-foreground">
            No definitions yet.{canWrite ? " Create one to get started." : ""}
          </p>
        )}
        {defs.map((d) => (
          <button
            key={d.id}
            onClick={() => open(d)}
            className={cn(
              "flex w-full items-start gap-2 rounded-lg px-2.5 py-2 text-left text-sm transition-colors",
              selected === d.name ? "bg-muted" : "hover:bg-muted/60",
            )}
          >
            <Bot className="mt-0.5 size-3.5 shrink-0 text-primary" />
            <span className="min-w-0 flex-1">
              <span className="flex items-center gap-1.5">
                <span className="truncate font-medium">{d.name}</span>
                <Badge variant="outline" className="h-4 shrink-0 px-1 text-[10px]">
                  v{d.current_version}
                </Badge>
              </span>
              <span className="mt-0.5 line-clamp-2 block text-xs text-muted-foreground">
                {d.description}
              </span>
            </span>
          </button>
        ))}
      </div>

      <div className="min-w-0">
        {selected === null ? (
          <div className="flex h-full items-center justify-center rounded-lg border border-dashed border-border py-16 text-sm text-muted-foreground">
            Select a definition{canWrite ? ", or create a new one" : ""} to edit.
          </div>
        ) : (
          <div className="space-y-3">
            <div className="flex items-center justify-between gap-2">
              <div className="min-w-0 text-sm font-medium">
                {originalName ?? "New agent"}
                {current?.model && (
                  <span className="ml-2 text-xs text-muted-foreground">model: {current.model}</span>
                )}
              </div>
              <div className="flex shrink-0 items-center gap-2">
                {originalName && canWrite && (
                  <ConfirmButton
                    title={`Delete ${originalName}?`}
                    description="This removes the definition and its version history. Agents already running are unaffected."
                    confirmLabel="Delete"
                    onConfirm={() => remove(originalName)}
                    trigger={
                      <Button size="sm" variant="outline">
                        <Trash2 className="size-3.5" />
                        Delete
                      </Button>
                    }
                  />
                )}
                {canWrite && (
                  <Button size="sm" onClick={save} disabled={busy || draft.trim() === ""}>
                    {busy && <Loader2 className="size-3.5 animate-spin" />}
                    Save
                  </Button>
                )}
              </div>
            </div>

            {saveError && <ErrorNotice message={saveError} />}
            {warnings.map((w, i) => (
              <p key={i} className="rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-amber-600 dark:text-amber-400">
                {w}
              </p>
            ))}

            <Textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              readOnly={!canWrite}
              spellCheck={false}
              className="min-h-[420px] font-mono text-xs"
            />
            <details className="text-xs text-muted-foreground">
              <summary className="cursor-pointer select-none">Frontmatter reference</summary>
              <div className="mt-1 space-y-1 rounded-lg bg-muted/50 p-3 font-mono">
                <div>name: required; the subagent_type the model picks</div>
                <div>description: required; when to use this agent</div>
                <div>tools: comma-separated allow-list (omit or * to inherit the run's pool)</div>
                <div>disallowedTools: removed after allow resolution</div>
                <div>model: model override (empty inherits the parent run's)</div>
                <div>maxTurns: child loop iteration cap</div>
                <div>skills: skill names whose scripts the child may run</div>
                <div>The body below the second --- is the child's system prompt.</div>
              </div>
            </details>
          </div>
        )}
      </div>
    </div>
  );
}
