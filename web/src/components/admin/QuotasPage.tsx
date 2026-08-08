// Platform quota configuration (enterprise-readiness P1-1 management face).
// A budget is the lever that makes usage accounting bite: set a monthly token
// cap on an account or a team and the quota checker rejects that scope's runs
// at submit once it crosses the cap. This page sets, shows, and clears those
// caps. It sits behind the platform-admin guard, alongside Users / Usage /
// Audit, because a budget throttles someone else's spend.

import { useState } from "react";
import { Gauge, Search, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { clearQuota, getQuota, putQuota, type QuotaBudget } from "@/lib/admin";
import {
  ErrorNotice,
  formatCount,
  formatDateTime,
  PageHeader,
} from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";

type Scope = "user" | "team";

export function PlatformQuotasPage() {
  const [scope, setScope] = useState<Scope>("user");
  const [ownerId, setOwnerId] = useState("");
  const [limit, setLimit] = useState("");

  // `result` is the last looked-up state: the budget for (scope, owner), or
  // null for "looked up, no cap set". `searched` distinguishes "nothing looked
  // up yet" from "looked up and found no cap".
  const [result, setResult] = useState<QuotaBudget | null>(null);
  const [searched, setSearched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const lookup = async (e?: React.FormEvent) => {
    e?.preventDefault();
    const id = ownerId.trim();
    if (id === "") return;
    setBusy(true);
    setError(null);
    try {
      const res = await getQuota(scope, id);
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

  const save = () =>
    act(async () => {
      const res = await putQuota({
        scope,
        owner_id: ownerId.trim(),
        monthly_tokens: parsedLimit,
      });
      return res.budget;
    });

  const ownerLabel = scope === "user" ? "Account id" : "Team id";
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
            Pick a scope and enter its {ownerLabel.toLowerCase()} to read or edit
            its monthly cap.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <form onSubmit={lookup} className="flex flex-wrap items-end gap-2">
            <div className="space-y-1.5">
              <Label htmlFor="quota-scope">Scope</Label>
              <NativeSelect
                id="quota-scope"
                value={scope}
                onChange={(e) => {
                  setScope(e.target.value as Scope);
                  setSearched(false);
                  setResult(null);
                }}
              >
                <NativeSelectOption value="user">Account</NativeSelectOption>
                <NativeSelectOption value="team">Team</NativeSelectOption>
              </NativeSelect>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="quota-owner">{ownerLabel}</Label>
              <Input
                id="quota-owner"
                value={ownerId}
                onChange={(e) => setOwnerId(e.target.value)}
                placeholder="Exact id"
                className="w-80 font-mono text-sm"
              />
            </div>
            <Button type="submit" variant="outline" size="sm" disabled={busy || ownerId.trim() === ""}>
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
                  void save();
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
                    description={`The ${scope === "user" ? "account" : "team"} will run uncapped again from the next run. This does not refund spend already counted this month.`}
                    confirmLabel="Clear cap"
                    onConfirm={() =>
                      act(() => clearQuota(scope, ownerId.trim()))
                    }
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
