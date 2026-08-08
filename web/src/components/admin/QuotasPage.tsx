// Platform quota configuration (enterprise-readiness P1-1 management face).
// A budget is the lever that makes usage accounting bite: set a monthly token
// cap on an account or a team and the quota checker rejects that scope's runs
// at submit once it crosses the cap. This page sets, shows, and clears those
// caps. It sits behind the platform-admin guard, alongside Users / Usage /
// Audit, because a budget throttles someone else's spend.
//
// The owner is picked from a searchable dropdown (accounts by email, teams by
// name), never typed as a raw id — the combobox carries the id for the API but
// shows a human label. Search is server-side (listUsers/listAllTeams `q`), so
// the list scales past what a client filter could hold.

import { useMemo, useRef, useState, useTransition } from "react";
import { Gauge, Search, Trash2 } from "lucide-react";
import { Combobox as ComboboxPrimitive } from "@base-ui/react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import {
  Combobox,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
} from "@/components/ui/combobox";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  clearQuota,
  getQuota,
  listAllTeams,
  listUsers,
  putQuota,
  type QuotaBudget,
} from "@/lib/admin";
import {
  ErrorNotice,
  formatCount,
  formatDateTime,
  PageHeader,
} from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";

type Scope = "user" | "team";

// Owner is one pickable quota target: an account or a team. `id` is what the
// quota API needs; the rest is for display and filtering.
type Owner = {
  id: string;
  // label is the primary text (email for an account, name for a team);
  // sublabel is a secondary hint (display name / member count).
  label: string;
  sublabel?: string;
};

export function PlatformQuotasPage() {
  const [scope, setScope] = useState<Scope>("user");
  const [owner, setOwner] = useState<Owner | null>(null);
  const [limit, setLimit] = useState("");

  // `result` is the last looked-up state: the budget for the selected owner, or
  // null for "looked up, no cap set". `searched` distinguishes "nothing looked
  // up yet" from "looked up and found no cap".
  const [result, setResult] = useState<QuotaBudget | null>(null);
  const [searched, setSearched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const lookup = async (e?: React.FormEvent) => {
    e?.preventDefault();
    if (!owner) return;
    setBusy(true);
    setError(null);
    try {
      const res = await getQuota(scope, owner.id);
      setResult(res.budget);
      setSearched(true);
      // Pre-fill the limit field from the current cap so editing is a tweak,
      // not a retype.
      setLimit(res.budget ? String(res.budget.monthly_tokens) : "");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const act = async (fn: () => Promise<QuotaBudget | void>) => {
    setBusy(true);
    setError(null);
    try {
      const b = await fn();
      // A set returns the new budget; a clear returns nothing => no cap now.
      setResult(b === undefined ? null : b);
      setSearched(true);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const save = () => {
    if (!owner) return;
    act(async () => {
      const res = await putQuota({
        scope,
        owner_id: owner.id,
        monthly_tokens: parsedLimit,
      });
      return res.budget;
    });
  };

  const clear = () => {
    if (!owner) return;
    act(() => clearQuota(scope, owner.id));
  };

  // Changing scope invalidates the picked owner (an account id is not a team
  // id) and any looked-up budget for the old scope.
  const changeScope = (next: Scope) => {
    setScope(next);
    setOwner(null);
    setResult(null);
    setSearched(false);
    setError(null);
  };

  const parsedLimit = Number(limit);

  return (
    <>
      <PageHeader
        title="Quotas"
        description="Monthly token budgets. A scope with a budget is rejected at run submit once it crosses its cap that month (HTTP 429); a scope with none runs uncapped. Account budgets cap one user; team budgets cap the spend billed to a team's provider key."
      />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Gauge className="size-4" />
            Look up a budget
          </CardTitle>
          <CardDescription>
            Pick a scope, then search for the{" "}
            {scope === "user" ? "account by email or display name" : "team by name"}{" "}
            to read or edit its monthly cap.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <form onSubmit={lookup} className="flex flex-wrap items-end gap-2">
            <div className="space-y-1.5">
              <Label htmlFor="quota-scope">Scope</Label>
              <NativeSelect
                id="quota-scope"
                value={scope}
                onChange={(e) => changeScope(e.target.value as Scope)}
              >
                <NativeSelectOption value="user">Account</NativeSelectOption>
                <NativeSelectOption value="team">Team</NativeSelectOption>
              </NativeSelect>
            </div>
            <div className="space-y-1.5">
              <Label>{scope === "user" ? "Account" : "Team"}</Label>
              <OwnerPicker scope={scope} value={owner} onChange={setOwner} />
            </div>
            <Button type="submit" variant="outline" size="sm" disabled={busy || !owner}>
              <Search />
              {busy ? "Loading…" : "Look up"}
            </Button>
          </form>

          {error && <ErrorNotice message={error} />}

          {searched && !error && (
            <div className="space-y-4 border-t border-border pt-4">
              <div className="flex items-center gap-2 text-sm">
                <span className="text-muted-foreground">Current cap:</span>
                {result ? (
                  <>
                    <Badge variant="secondary" className="tabular-nums">
                      {formatCount(result.monthly_tokens)} tokens / month
                    </Badge>
                    <span className="text-xs text-muted-foreground">
                      set {formatDateTime(result.updated_at)}
                    </span>
                  </>
                ) : (
                  <Badge variant="outline">No cap — uncapped</Badge>
                )}
              </div>

              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  save();
                }}
                className="flex flex-wrap items-end gap-2"
              >
                <div className="space-y-1.5">
                  <Label htmlFor="quota-limit">Monthly tokens</Label>
                  <Input
                    id="quota-limit"
                    type="number"
                    min={1}
                    inputMode="numeric"
                    value={limit}
                    onChange={(e) => setLimit(e.target.value)}
                    placeholder="e.g. 1000000"
                    className="w-56 tabular-nums"
                  />
                  <p className="text-xs text-muted-foreground">
                    Billable tokens (input + output) per calendar month.
                  </p>
                </div>
                <Button
                  type="submit"
                  size="sm"
                  disabled={busy || !Number.isFinite(parsedLimit) || parsedLimit <= 0}
                >
                  {result ? "Update cap" : "Set cap"}
                </Button>
                {result && (
                  <ConfirmButton
                    title="Clear this budget?"
                    description={`${scope === "user" ? "This account" : "This team"} will run uncapped again from the next run. This does not refund spend already counted this month.`}
                    confirmLabel="Clear cap"
                    onConfirm={clear}
                    trigger={
                      <Button variant="ghost" size="sm" disabled={busy}>
                        <Trash2 />
                        Clear cap
                      </Button>
                    }
                  />
                )}
              </form>
            </div>
          )}
        </CardContent>
      </Card>
    </>
  );
}

// OwnerPicker is the searchable account/team dropdown. It fetches matches from
// the platform list endpoints as the operator types (server-side `q` filter),
// aborting a superseded request, and keeps the picked owner in the items list
// so it stays selected while results for a new query stream in.
function OwnerPicker({
  scope,
  value,
  onChange,
}: {
  scope: Scope;
  value: Owner | null;
  onChange: (o: Owner | null) => void;
}) {
  const [results, setResults] = useState<Owner[]>([]);
  const [searchValue, setSearchValue] = useState("");
  const [searchError, setSearchError] = useState<string | null>(null);
  const [isPending, startTransition] = useTransition();
  const abortRef = useRef<AbortController | null>(null);

  const trimmed = searchValue.trim();

  // Keep the selected owner in the list so it renders as the selected item even
  // after a new query narrows the results away from it.
  const items = useMemo(() => {
    if (!value || results.some((o) => o.id === value.id)) return results;
    return [value, ...results];
  }, [results, value]);

  const status = isPending
    ? "Searching…"
    : searchError
      ? searchError
      : trimmed === ""
        ? value
          ? null
          : `Type to search ${scope === "user" ? "accounts" : "teams"}…`
        : results.length === 0
          ? `No ${scope === "user" ? "account" : "team"} matches "${trimmed}".`
          : null;

  return (
    <Combobox
      items={items}
      itemToStringLabel={(o: Owner) => o.label}
      filter={null}
      value={value}
      onValueChange={(next: Owner | null) => {
        onChange(next);
        setSearchValue("");
        setSearchError(null);
      }}
      onOpenChangeComplete={(open) => {
        // When the popup closes with a selection, show just that selection so
        // reopening starts from it rather than a stale result list.
        if (!open && value) setResults([value]);
      }}
      onInputValueChange={(next: string, { reason }) => {
        setSearchValue(next);
        if (next === "") {
          setResults([]);
          setSearchError(null);
          return;
        }
        // An item press re-fills the input with the label; don't search for it.
        if (reason === "item-press") return;

        const controller = new AbortController();
        abortRef.current?.abort();
        abortRef.current = controller;

        startTransition(async () => {
          setSearchError(null);
          try {
            const owners =
              scope === "user"
                ? (await listUsers({ q: next, limit: 20 })).users.map(
                    (u): Owner => ({
                      id: u.id,
                      label: u.email,
                      sublabel: u.display_name || undefined,
                    }),
                  )
                : (await listAllTeams({ q: next, limit: 20 })).teams.map(
                    (t): Owner => ({
                      id: t.id,
                      label: t.name,
                      sublabel:
                        t.members === undefined ? undefined : `${t.members} members`,
                    }),
                  );
            if (controller.signal.aborted) return;
            startTransition(() => setResults(owners));
          } catch (err) {
            if (controller.signal.aborted) return;
            startTransition(() => setSearchError((err as Error).message));
          }
        });
      }}
    >
      <ComboboxInput
        placeholder={scope === "user" ? "Search accounts…" : "Search teams…"}
        showClear
        className="w-80"
      />
      <ComboboxContent>
        <ComboboxPrimitive.Status className="flex items-center gap-2 px-2 py-1.5 text-xs text-muted-foreground data-empty:hidden">
          {status}
        </ComboboxPrimitive.Status>
        <ComboboxEmpty>
          {trimmed !== "" && !isPending && results.length === 0 && !searchError
            ? "Try a different search."
            : null}
        </ComboboxEmpty>
        <ComboboxList>
          {(o: Owner) => (
            <ComboboxItem key={o.id} value={o}>
              <span className="flex min-w-0 flex-col">
                <span className="truncate">{o.label}</span>
                {o.sublabel && (
                  <span className="truncate text-xs text-muted-foreground">
                    {o.sublabel}
                  </span>
                )}
              </span>
            </ComboboxItem>
          )}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}
