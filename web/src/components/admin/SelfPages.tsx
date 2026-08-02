// "My usage" and "My memories" — the read-and-clean views over the caller's own
// tokens and long-term memory.

import { useEffect, useState, type ReactNode } from "react";
import { Loader2, Sparkles, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  deleteMyMemory,
  dreamStatus,
  myMemories,
  myUsage,
  triggerDream,
  type DreamState,
  type Memory,
} from "@/lib/admin";
import { ApiError } from "@/lib/api";
import {
  AsyncSection,
  ErrorNotice,
  formatCount,
  formatDate,
  formatDateTime,
  PageHeader,
  TokenStats,
  useAsync,
} from "@/components/admin/common";
import { DateRangePicker, UsageTrend, useDateRange } from "@/components/admin/usage-parts";
import { ConfirmButton } from "@/components/admin/confirm";

export function MyUsagePage() {
  const { range, setRange } = useDateRange();
  const state = useAsync(() => myUsage(range), [range.from, range.to]);

  return (
    <>
      <PageHeader
        title="My usage"
        description="Tokens consumed by runs in your own sessions. Counts only — the platform does not record which model produced a run, so there is no cost figure."
        actions={<DateRangePicker range={range} onChange={setRange} />}
      />
      <AsyncSection state={state} loadingLabel="Loading usage">
        {(data) => (
          <div className="space-y-6">
            <TokenStats tokens={data.total} />
            <UsageTrend rows={data.daily} />
          </div>
        )}
      </AsyncSection>
    </>
  );
}

export function MyMemoriesPage() {
  const state = useAsync(() => myMemories(), []);
  const [error, setError] = useState<string | null>(null);
  const dream = useDreaming(state.reload);

  const remove = async (m: Memory) => {
    setError(null);
    try {
      await deleteMyMemory(m.id);
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  return (
    <>
      <PageHeader
        title="My memories"
        description="What the agent has remembered about you across sessions. Deleting a memory is permanent."
        actions={
          dream.available && (
            <Button onClick={dream.trigger} disabled={dream.running} variant="outline">
              {dream.running ? (
                <>
                  <Loader2 className="animate-spin" />
                  Consolidating
                </>
              ) : (
                <>
                  <Sparkles />
                  Consolidate now
                </>
              )}
            </Button>
          )
        }
      />
      {error && <ErrorNotice message={error} />}
      {dream.error && <ErrorNotice message={dream.error} />}
      <DreamNote state={dream.state} running={dream.running} />
      <AsyncSection state={state} loadingLabel="Loading memories">
        {(data) => (
          <MemoryTable
            memories={data.memories}
            emptyMessage="Nothing remembered yet. Memories accumulate as the dreaming worker consolidates your sessions."
            onDelete={remove}
          />
        )}
      </AsyncSection>
    </>
  );
}

// useDreaming drives the manual consolidation control: current state, the
// trigger, and polling while a pass is in flight.
//
// It polls rather than streams because a pass is a minutes-long background job
// with one bit of interesting state — a socket for that would cost more than it
// saves. `onFinished` fires on the running→idle edge so the memory list
// refreshes with what the pass produced.
function useDreaming(onFinished: () => void) {
  const [state, setState] = useState<DreamState | null>(null);
  const [available, setAvailable] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    dreamStatus()
      .then((s) => {
        if (!cancelled) setState(s);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        // 503 means no worker is wired — the deployment has no provider
        // configured. Hide the control rather than offering a button that
        // cannot work, or an error the reader cannot act on.
        if (e instanceof ApiError && e.status === 503) setAvailable(false);
        else setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const running = state?.running ?? false;

  useEffect(() => {
    if (!running) return;
    let cancelled = false;
    const id = window.setInterval(() => {
      dreamStatus()
        .then((next) => {
          if (cancelled) return;
          setState(next);
          if (!next.running) onFinished();
        })
        // A failed poll is not worth surfacing: the pass is still running on the
        // server and the next tick asks again.
        .catch(() => {});
    }, 2500);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [running, onFinished]);

  const trigger = async () => {
    setError(null);
    try {
      setState(await triggerDream());
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return { available, state, running, error, trigger };
}

// DreamNote reports what the last manual pass did. The counts are separated
// because they mean different things: `added` grew the store, `revised` rewrote
// memories in place, and `retired` took memories out of recall.
function DreamNote({ state, running }: { state: DreamState | null; running: boolean }) {
  if (running) {
    return (
      <p className="text-sm text-muted-foreground">
        Consolidating your sessions. This runs in the background — you can leave this page.
      </p>
    );
  }
  const last = state?.last;
  if (!last) return null;
  if (last.error) {
    return <ErrorNotice message={`Last consolidation failed: ${last.error}`} />;
  }

  const changed = last.added + last.revised + last.retired;
  // A compaction pass reviewed the existing store rather than learning from new
  // conversations. Reporting it as "nothing new to consolidate" would describe
  // the input and hide the work.
  const lead = last.compacted
    ? "Reviewed your existing memories"
    : `${formatCount(last.episodes)} messages read`;

  if (last.compacted && changed === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        Last run {formatDateTime(last.finished_at)}: reviewed your existing memories, nothing to
        merge or retire.
      </p>
    );
  }
  if (!last.compacted && last.episodes === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        Last run {formatDateTime(last.finished_at)}: nothing to consolidate.
      </p>
    );
  }
  return (
    <p className="text-sm text-muted-foreground">
      Last run {formatDateTime(last.finished_at)}: {lead}
      {" · "}
      {formatCount(last.added)} added, {formatCount(last.revised)} revised,{" "}
      {formatCount(last.retired)} retired
      {last.purged > 0 && ` · ${formatCount(last.purged)} purged`}
      {" · "}
      {formatCount(last.tokens)} tokens
      {last.budget_exhausted &&
        " · stopped at the token budget; the rest is queued for the next run"}
    </p>
  );
}

// MemoryTable is shared by the self, team, and platform memory views. onDeprecate
// is optional: only team and platform callers can supersede a memory without
// erasing it.
//
// Superseded memories are hidden by default. They are excluded from recall, so
// they are not part of what the agent knows — showing them mixed into the live
// set misrepresents the store, and the dreaming worker retires enough of them
// that they can outnumber the memories that still count. They stay one click
// away because superseding is reversible until the purge window closes.
export function MemoryTable({
  memories,
  emptyMessage,
  onDelete,
  onDeprecate,
  readOnly,
}: {
  memories: Memory[];
  emptyMessage: string;
  onDelete?: (m: Memory) => void | Promise<void>;
  onDeprecate?: (m: Memory) => void | Promise<void>;
  readOnly?: boolean;
}) {
  const [showSuperseded, setShowSuperseded] = useState(false);

  const superseded = memories.filter((m) => m.deprecated).length;
  const live = memories.length - superseded;
  const rows = showSuperseded ? memories : memories.filter((m) => !m.deprecated);

  if (memories.length === 0) {
    return <EmptyNote>{emptyMessage}</EmptyNote>;
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          {formatCount(live)} live
          {superseded > 0 && ` · ${formatCount(superseded)} superseded`}
        </p>
        {superseded > 0 && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setShowSuperseded((v) => !v)}
            aria-pressed={showSuperseded}
          >
            {showSuperseded ? "Hide superseded" : "Show superseded"}
          </Button>
        )}
      </div>

      {rows.length === 0 ? (
        // Every memory here is superseded. Falling through to emptyMessage would
        // claim nothing was ever remembered, which is the opposite of true.
        <EmptyNote>
          Every memory in this scope has been superseded. Show them to review or delete them.
        </EmptyNote>
      ) : (
        <MemoryRows
          rows={rows}
          onDelete={onDelete}
          onDeprecate={onDeprecate}
          readOnly={readOnly}
        />
      )}
    </div>
  );
}

function EmptyNote({ children }: { children: ReactNode }) {
  return (
    <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
      {children}
    </p>
  );
}

function MemoryRows({
  rows,
  onDelete,
  onDeprecate,
  readOnly,
}: {
  rows: Memory[];
  onDelete?: (m: Memory) => void | Promise<void>;
  onDeprecate?: (m: Memory) => void | Promise<void>;
  readOnly?: boolean;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead className="w-28">Kind</TableHead>
          <TableHead>Content</TableHead>
          <TableHead className="w-28">Created</TableHead>
          {!readOnly && <TableHead className="w-40" />}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((m) => (
          <TableRow key={m.id} className={m.deprecated ? "opacity-60" : undefined}>
            <TableCell>
              <Badge variant="outline" className="capitalize">
                {m.kind}
              </Badge>
            </TableCell>
            {/* whitespace-normal is required: TableCell sets whitespace-nowrap,
                which makes max-w-* a no-op — the text overflows the cell and
                paints over the Created column instead of wrapping. */}
            <TableCell className="max-w-md whitespace-normal">
              <span className="text-sm">{m.content}</span>
              {m.deprecated && (
                <Badge variant="secondary" className="ml-2">
                  Superseded
                </Badge>
              )}
            </TableCell>
            <TableCell className="text-sm text-muted-foreground">
              {formatDate(m.created_at)}
            </TableCell>
            {!readOnly && (
              <TableCell className="text-right">
                <div className="flex justify-end gap-1">
                  {onDeprecate && !m.deprecated && (
                    <Button variant="ghost" size="sm" onClick={() => void onDeprecate(m)}>
                      Supersede
                    </Button>
                  )}
                  {onDelete && (
                    <ConfirmButton
                      title="Delete this memory?"
                      description="The memory is erased permanently. Superseding instead keeps the record but excludes it from recall."
                      confirmLabel="Delete"
                      onConfirm={() => onDelete(m)}
                      trigger={
                        <Button variant="ghost" size="icon-sm" aria-label="Delete memory">
                          <Trash2 />
                        </Button>
                      }
                    />
                  )}
                </div>
              </TableCell>
            )}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// UsageRowsTable renders a grouped usage report (by account or by team).
export function UsageRowsTable({
  rows,
  groupLabel,
}: {
  rows: { id: string; label: string; tokens: { input: number; output: number; runs: number } }[];
  groupLabel: string;
}) {
  if (rows.length === 0) {
    return (
      <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
        No runs in this period.
      </p>
    );
  }
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{groupLabel}</TableHead>
          <TableHead className="text-right">Input</TableHead>
          <TableHead className="text-right">Output</TableHead>
          <TableHead className="text-right">Total</TableHead>
          <TableHead className="text-right">Runs</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((r) => (
          <TableRow key={r.id}>
            <TableCell className="font-medium">{r.label}</TableCell>
            <TableCell className="text-right tabular-nums">
              {formatCount(r.tokens.input)}
            </TableCell>
            <TableCell className="text-right tabular-nums">
              {formatCount(r.tokens.output)}
            </TableCell>
            <TableCell className="text-right font-medium tabular-nums">
              {formatCount(r.tokens.input + r.tokens.output)}
            </TableCell>
            <TableCell className="text-right tabular-nums text-muted-foreground">
              {r.tokens.runs}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
