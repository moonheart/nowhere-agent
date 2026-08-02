// Small pieces every console page repeats: async data loading with its three
// states, a page frame, token formatting, and the role badge.

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { AlertCircle, Loader2 } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import type { PlatformRole, TeamRole, Tokens } from "@/lib/admin";
import { cn } from "@/lib/utils";

// useAsync runs a loader and tracks its three states, discarding results from a
// request that a newer one has superseded. `reload` re-runs it after a mutation.
export function useAsync<T>(
  loader: () => Promise<T>,
  deps: unknown[],
): {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
  setData: (v: T) => void;
} {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [version, setVersion] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    loader()
      .then((v) => {
        if (!cancelled) {
          setData(v);
          setError(null);
        }
      })
      .catch((e: Error) => {
        if (!cancelled) setError(e.message);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // The caller owns the dependency list; loader is recreated each render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, version]);

  const reload = useCallback(() => setVersion((v) => v + 1), []);
  return { data, loading, error, reload, setData };
}

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3 border-b border-border pb-4">
      <div className="space-y-1">
        <h1 className="text-lg font-semibold tracking-tight text-foreground">
          {title}
        </h1>
        {description && (
          <p className="max-w-2xl text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  );
}

export function ErrorNotice({ message }: { message: string }) {
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>Something went wrong</AlertTitle>
      <AlertDescription>{message}</AlertDescription>
    </Alert>
  );
}

export function Loading({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin" />
      {label}…
    </div>
  );
}

// AsyncSection renders the loading and error states once, so each page's body
// only has to handle the loaded case.
export function AsyncSection<T>({
  state,
  children,
  loadingLabel,
}: {
  state: { data: T | null; loading: boolean; error: string | null };
  children: (data: T) => ReactNode;
  loadingLabel?: string;
}) {
  if (state.loading && state.data === null) return <Loading label={loadingLabel} />;
  if (state.error) return <ErrorNotice message={state.error} />;
  if (state.data === null) return null;
  return <>{children(state.data)}</>;
}

// formatCount renders a token count compactly — six-digit numbers are common
// and unreadable in full inside a table cell.
export function formatCount(n: number): string {
  if (n < 1_000) return String(n);
  if (n < 1_000_000) return `${(n / 1_000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export function formatDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

export function RoleBadge({ role }: { role: TeamRole }) {
  return (
    <Badge
      variant={role === "owner" ? "default" : role === "admin" ? "secondary" : "outline"}
      className="capitalize"
    >
      {role}
    </Badge>
  );
}

export function PlatformRoleBadge({ role }: { role: PlatformRole }) {
  if (role !== "admin") {
    return <span className="text-sm text-muted-foreground">User</span>;
  }
  return <Badge variant="default">Administrator</Badge>;
}

// StatTile is one number in a summary row. `hint` explains what the number
// counts, since "input" and "cache read" are not self-evident.
export function StatTile({
  label,
  value,
  hint,
  className,
}: {
  label: string;
  value: string | number;
  hint?: string;
  className?: string;
}) {
  return (
    <Card className={cn("gap-0 py-4", className)}>
      <CardContent className="px-4">
        <div className="text-xs font-medium tracking-wide text-muted-foreground uppercase">
          {label}
        </div>
        <div className="mt-1 text-2xl font-semibold tabular-nums text-foreground">
          {value}
        </div>
        {hint && <div className="mt-1 text-xs text-muted-foreground">{hint}</div>}
      </CardContent>
    </Card>
  );
}

// TokenStats is the four-counter summary shared by every usage view. Input and
// output are shown as the billable pair; cache read/write sit beside them
// because providers price them differently and folding them in would misstate
// both.
export function TokenStats({ tokens }: { tokens: Tokens }) {
  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
      <StatTile
        label="Total"
        value={formatCount(tokens.input + tokens.output)}
        hint="input + output"
      />
      <StatTile label="Input" value={formatCount(tokens.input)} />
      <StatTile label="Output" value={formatCount(tokens.output)} />
      <StatTile
        label="Cache read"
        value={formatCount(tokens.cache_read)}
        hint="prompt-prefix hits"
      />
      <StatTile label="Runs" value={formatCount(tokens.runs)} />
    </div>
  );
}
