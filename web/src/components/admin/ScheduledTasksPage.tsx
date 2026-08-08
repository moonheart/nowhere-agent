// Scheduled tasks (scheduled-tasks): the caller's recurring agent runs. A list
// with enable/disable and delete, plus a create/edit dialog covering the cron
// schedule, timezone, prompt source (free text or an agent definition), and the
// front-loaded tool whitelist the fired run is confined to. The sessions a task
// produced open in a dialog that deep-links into the chat view.

import { useState } from "react";
import { Link } from "react-router-dom";
import {
  CalendarClock,
  ExternalLink,
  Loader2,
  Pencil,
  Play,
  Plus,
  Rows3,
  Trash2,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import {
  createScheduledTask,
  deleteScheduledTask,
  disableScheduledTask,
  enableScheduledTask,
  clearTaskSessions,
  listScheduledTasks,
  runScheduledTask,
  taskSessions,
  updateScheduledTask,
  type MultitaskStrategy,
  type OnRunCompleted,
  type ProducedSession,
  type ScheduledTask,
  type ScheduledTaskInput,
} from "@/lib/scheduled-tasks";
import {
  AsyncSection,
  ErrorNotice,
  formatDateTime,
  PageHeader,
  useAsync,
} from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";

// The whitelist picker offers the built-in tool names. It is static because
// there is no endpoint that enumerates the registry; an unknown name typed by
// the backend is simply not bound at fire time. Kept in sync with the tools
// cmd/server wires into buildToolRegistry.
const KNOWN_TOOLS: { name: string; hint: string }[] = [
  { name: "read_file", hint: "read files in the workspace" },
  { name: "list_dir", hint: "list directories" },
  { name: "grep", hint: "search file contents" },
  { name: "glob", hint: "find files by pattern" },
  { name: "write_file", hint: "create / overwrite files" },
  { name: "edit_file", hint: "edit files in place" },
  { name: "move_file", hint: "move / rename files" },
  { name: "copy_file", hint: "copy files" },
  { name: "delete_file", hint: "delete files" },
  { name: "make_dir", hint: "create directories" },
  { name: "run_command", hint: "run shell commands" },
  { name: "recall_memory", hint: "search long-term memory" },
  { name: "load_skill", hint: "load skill instructions" },
  { name: "run_skill_script", hint: "run a skill's scripts" },
  { name: "spawn_agent", hint: "launch subagents" },
  { name: "plan_write", hint: "maintain a task plan" },
  { name: "ask_user", hint: "ask the user (never answered when unattended)" },
];

export function ScheduledTasksPage() {
  const state = useAsync(() => listScheduledTasks(), []);
  const [error, setError] = useState<string | null>(null);

  const toggle = async (t: ScheduledTask, enabled: boolean) => {
    setError(null);
    try {
      if (enabled) await enableScheduledTask(t.id);
      else await disableScheduledTask(t.id);
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const remove = async (t: ScheduledTask) => {
    setError(null);
    try {
      await deleteScheduledTask(t.id);
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  // Ids of tasks currently being fired manually, so the button spins and can't
  // be double-clicked while a run is being submitted.
  const [running, setRunning] = useState<Set<string>>(new Set());
  const runNow = async (t: ScheduledTask) => {
    setError(null);
    setRunning((s) => new Set(s).add(t.id));
    try {
      await runScheduledTask(t.id);
      state.reload(); // last_run_at / produced sessions may have changed
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setRunning((s) => {
        const next = new Set(s);
        next.delete(t.id);
        return next;
      });
    }
  };

  return (
    <>
      <PageHeader
        title="Scheduled tasks"
        description="Recurring agent runs that fire on a cron schedule, unattended. Each run binds only the tools on its whitelist — there is no one to approve a risky call at 4am."
        actions={<TaskFormDialog onSaved={state.reload} />}
      />
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading tasks">
        {(data) =>
          // The API serializes an empty result set as [] (never null), but guard
          // anyway — a null here would otherwise throw on .length and blank the page.
          (data.tasks ?? []).length === 0 ? (
            <p className="rounded-lg border border-dashed border-border px-4 py-10 text-center text-sm text-muted-foreground">
              No scheduled tasks yet. Create one to run a prompt on a schedule.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Task</TableHead>
                  <TableHead className="w-40">Schedule</TableHead>
                  <TableHead className="w-40">Next run</TableHead>
                  <TableHead className="w-24">Tools</TableHead>
                  <TableHead className="w-28">Enabled</TableHead>
                  <TableHead className="w-32" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data.tasks ?? []).map((t) => (
                  <TableRow key={t.id} className={t.enabled ? undefined : "opacity-60"}>
                    <TableCell className="max-w-md whitespace-normal">
                      <TaskLabel task={t} />
                    </TableCell>
                    <TableCell>
                      <div className="text-sm font-medium tabular-nums">{t.cron}</div>
                      <div className="text-xs text-muted-foreground">{t.timezone}</div>
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {t.enabled ? formatDateTime(t.next_run_at) : "—"}
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline">{(t.tool_whitelist ?? []).length}</Badge>
                    </TableCell>
                    <TableCell>
                      <Switch
                        aria-label={t.enabled ? "Disable task" : "Enable task"}
                        checked={t.enabled}
                        onCheckedChange={(v) => void toggle(t, v)}
                      />
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label="Run now"
                          title="Run now (does not change the schedule)"
                          disabled={running.has(t.id)}
                          onClick={() => void runNow(t)}
                        >
                          {running.has(t.id) ? <Loader2 className="animate-spin" /> : <Play />}
                        </Button>
                        <TaskSessionsDialog task={t} />
                        <TaskFormDialog task={t} onSaved={state.reload} />
                        <ConfirmButton
                          title="Delete this task?"
                          description="The schedule is removed and the task stops firing. Sessions it already produced are kept."
                          confirmLabel="Delete"
                          onConfirm={() => remove(t)}
                          trigger={
                            <Button variant="ghost" size="icon-sm" aria-label="Delete task">
                              <Trash2 />
                            </Button>
                          }
                        />
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )
        }
      </AsyncSection>
    </>
  );
}

// TaskLabel shows what runs: the prompt (or the agent definition name) plus a
// note when the run appends to a fixed session.
function TaskLabel({ task }: { task: ScheduledTask }) {
  return (
    <div className="space-y-0.5">
      <div className="text-sm font-medium">
        {task.agent_def_name ? (
          <span className="inline-flex items-center gap-1.5">
            <Badge variant="secondary">{task.agent_def_name}</Badge>
            {task.prompt && <span className="text-muted-foreground">{task.prompt}</span>}
          </span>
        ) : (
          task.prompt
        )}
      </div>
      {task.target_session_id && (
        <div className="text-xs text-muted-foreground">appends to a fixed session</div>
      )}
    </div>
  );
}

// ---- form ----

type FormState = {
  source: "prompt" | "agentdef";
  prompt: string;
  agentDefName: string;
  cron: string;
  timezone: string;
  targetSessionId: string;
  onRunCompleted: OnRunCompleted;
  multitask: MultitaskStrategy;
  endTime: string; // datetime-local value; "" = no expiry
  whitelist: string[];
};

function formFrom(task?: ScheduledTask): FormState {
  return {
    source: task?.agent_def_name ? "agentdef" : "prompt",
    prompt: task?.prompt ?? "",
    agentDefName: task?.agent_def_name ?? "",
    cron: task?.cron ?? "0 9 * * *",
    timezone: task?.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone ?? "UTC",
    targetSessionId: task?.target_session_id ?? "",
    onRunCompleted: task?.on_run_completed ?? "keep",
    multitask: task?.multitask_strategy ?? "reject",
    endTime: task?.end_time ? toLocalInput(task.end_time) : "",
    whitelist: task?.tool_whitelist ?? ["read_file", "list_dir", "grep", "glob"],  };
}

// toLocalInput converts an ISO timestamp to a datetime-local input value.
function toLocalInput(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function TaskFormDialog({
  task,
  onSaved,
}: {
  task?: ScheduledTask; // set = edit, unset = create
  onSaved: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<FormState>(() => formFrom(task));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) =>
    setForm((f) => ({ ...f, [k]: v }));

  const toggleTool = (name: string, on: boolean) =>
    setForm((f) => ({
      ...f,
      whitelist: on ? [...f.whitelist, name] : f.whitelist.filter((n) => n !== name),
    }));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    const body: ScheduledTaskInput = {
      prompt: form.source === "prompt" ? form.prompt.trim() : form.prompt.trim() || undefined,
      agent_def_name: form.source === "agentdef" ? form.agentDefName.trim() : undefined,
      cron: form.cron.trim(),
      timezone: form.timezone.trim() || "UTC",
      tool_whitelist: form.whitelist,
      target_session_id: form.targetSessionId.trim() || undefined,
      on_run_completed: form.onRunCompleted,
      multitask_strategy: form.multitask,
      end_time: form.endTime ? new Date(form.endTime).toISOString() : undefined,
    };
    try {
      if (task) await updateScheduledTask(task.id, body);
      else await createScheduledTask(body);
      setOpen(false);
      onSaved();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // A free-text task needs a prompt; an agent-def task needs the definition
  // name (its prompt is an optional kickoff).
  const valid =
    form.cron.trim() !== "" &&
    (form.source === "prompt" ? form.prompt.trim() !== "" : form.agentDefName.trim() !== "");

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (o) setForm(formFrom(task)); // reset to the task's current values each open
      }}
    >
      <DialogTrigger
        render={
          task ? (
            <Button variant="ghost" size="icon-sm" aria-label="Edit task">
              <Pencil />
            </Button>
          ) : (
            <Button size="sm">
              <Plus />
              New task
            </Button>
          )
        }
      />
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{task ? "Edit task" : "New scheduled task"}</DialogTitle>
            <DialogDescription>
              The run fires unattended, so grant only the tools it needs.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-5 py-4">
            {/* Prompt source */}
            <div className="space-y-2">
              <Label>What runs</Label>
              <div className="flex gap-2">
                <Button
                  type="button"
                  variant={form.source === "prompt" ? "default" : "outline"}
                  size="sm"
                  onClick={() => set("source", "prompt")}
                >
                  Free-text prompt
                </Button>
                <Button
                  type="button"
                  variant={form.source === "agentdef" ? "default" : "outline"}
                  size="sm"
                  onClick={() => set("source", "agentdef")}
                >
                  Agent definition
                </Button>
              </div>
              {form.source === "agentdef" && (
                <Input
                  value={form.agentDefName}
                  onChange={(e) => set("agentDefName", e.target.value)}
                  placeholder="agent type name (e.g. general-purpose)"
                  className="mt-2"
                />
              )}
              <Textarea
                value={form.prompt}
                onChange={(e) => set("prompt", e.target.value)}
                placeholder={
                  form.source === "agentdef"
                    ? "Opening instruction (optional — defaults to a standard kickoff)"
                    : "Every morning, summarize the new support tickets in the workspace…"
                }
                rows={3}
                className="mt-2"
              />
            </div>

            {/* Schedule */}
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="task-cron">Cron (min hour dom mon dow)</Label>
                <Input
                  id="task-cron"
                  value={form.cron}
                  onChange={(e) => set("cron", e.target.value)}
                  placeholder="0 9 * * *"
                  className="tabular-nums"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="task-tz">Timezone (IANA)</Label>
                <Input
                  id="task-tz"
                  value={form.timezone}
                  onChange={(e) => set("timezone", e.target.value)}
                  placeholder="UTC"
                />
              </div>
            </div>

            {/* Whitelist */}
            <div className="space-y-2">
              <Label>
                Tool whitelist{" "}
                <span className="font-normal text-muted-foreground">
                  ({form.whitelist.length} granted)
                </span>
              </Label>
              <div className="grid gap-1.5 rounded-lg border border-border p-3 sm:grid-cols-2">
                {KNOWN_TOOLS.map((t) => (
                  <label
                    key={t.name}
                    className="flex cursor-pointer items-center gap-2 rounded px-1 py-0.5 text-sm hover:bg-muted/60"
                  >
                    <Checkbox
                      checked={form.whitelist.includes(t.name)}
                      onCheckedChange={(v) => toggleTool(t.name, v === true)}
                    />
                    <span className="font-mono text-xs">{t.name}</span>
                    <span className="truncate text-xs text-muted-foreground">{t.hint}</span>
                  </label>
                ))}
              </div>
            </div>

            {/* Advanced */}
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="task-target">Target session (optional)</Label>
                <Input
                  id="task-target"
                  value={form.targetSessionId}
                  onChange={(e) => set("targetSessionId", e.target.value)}
                  placeholder="blank = a fresh session each run"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="task-end">Stop after (optional)</Label>
                <Input
                  id="task-end"
                  type="datetime-local"
                  value={form.endTime}
                  onChange={(e) => set("endTime", e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label>If the session is busy</Label>
                <Select
                  value={form.multitask}
                  onValueChange={(v) => set("multitask", (v ?? "reject") as MultitaskStrategy)}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="reject">Skip this run</SelectItem>
                    <SelectItem value="interrupt">Cancel the active run</SelectItem>
                    <SelectItem value="enqueue">Wait for the next slot</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>When a fresh-session run ends</Label>
                <Select
                  value={form.onRunCompleted}
                  onValueChange={(v) => set("onRunCompleted", (v ?? "keep") as OnRunCompleted)}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="keep">Keep the session</SelectItem>
                    <SelectItem value="delete">Delete the session</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
          </div>

          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || !valid}>
              {busy ? "Saving…" : task ? "Save changes" : "Create task"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- produced sessions ----

// TaskSessionsDialog lists the sessions a task produced and links each into the
// chat view (?session=<id>). A "clear all" control soft-deletes the whole set —
// they leave the sidebar but their rows stay for audit.
function TaskSessionsDialog({ task }: { task: ScheduledTask }) {
  const [open, setOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const state = useAsync(
    () => (open ? taskSessions(task.id) : Promise.resolve({ sessions: [] as ProducedSession[] })),
    [open, task.id],
  );

  const clearAll = async () => {
    setError(null);
    try {
      await clearTaskSessions(task.id);
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label="View produced sessions" title="Produced sessions">
            <Rows3 />
          </Button>
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <CalendarClock className="size-4" />
            Produced sessions
          </DialogTitle>
          <DialogDescription>
            Sessions this task created. Open one in a new tab to read the run.
          </DialogDescription>
        </DialogHeader>
        {error && <ErrorNotice message={error} />}
        {open && (
          <AsyncSection state={state} loadingLabel="Loading sessions">
            {(data) =>
              // Guard against a null sessions array (a task that has not fired).
              (data.sessions ?? []).length === 0 ? (
                <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                  No sessions yet — the task has not fired (or it appends to a fixed session).
                </p>
              ) : (
                <>
                  <div className="flex items-center justify-between gap-3 pb-1">
                    <p className="text-sm text-muted-foreground">
                      {(data.sessions ?? []).length} session{(data.sessions ?? []).length === 1 ? "" : "s"}
                    </p>
                    <ConfirmButton
                      title="Clear all produced sessions?"
                      description="Every session this task created is removed from your sidebar. Their contents stay on the server for audit, but they no longer appear here or in chat."
                      confirmLabel="Clear all"
                      onConfirm={clearAll}
                      trigger={
                        <Button variant="outline" size="sm">
                          <Trash2 />
                          Clear all
                        </Button>
                      }
                    />
                  </div>
                  <ul className="max-h-80 space-y-1 overflow-y-auto">
                    {(data.sessions ?? []).map((s) => (
                      <li key={s.id}>
                        <Link
                          to={`/?session=${encodeURIComponent(s.id)}`}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="flex items-center justify-between gap-3 rounded-lg px-3 py-2 hover:bg-muted/60"
                        >
                          <span className="flex min-w-0 items-center gap-2">
                            <span className="truncate text-sm font-medium">
                              {s.title?.trim() || "Untitled session"}
                            </span>
                            <ExternalLink className="size-3.5 shrink-0 text-muted-foreground" />
                          </span>
                          <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
                            {formatDateTime(s.created_at)}
                          </span>
                        </Link>
                      </li>
                    ))}
                  </ul>
                </>
              )
            }
          </AsyncSection>
        )}
      </DialogContent>
    </Dialog>
  );
}
