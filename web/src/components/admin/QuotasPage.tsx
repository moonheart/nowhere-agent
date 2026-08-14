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
import { t } from "@/lib/i18n";
import {
  ErrorNotice,
  formatCount,
  formatDateTime,
  PageHeader,
} from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";

export type Scope = "user" | "team";

// Owner is one pickable quota target: an account or a team. `id` is what the
// quota API needs; the rest is for display and filtering.
export type Owner = {
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
        title={t("quotasPage.title")}
        description={t("quotasPage.description")}
      />

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Gauge className="size-4" />
            {t("quotasPage.lookupTitle")}
          </CardTitle>
          <CardDescription>
            {scope === "user"
              ? t("quotasPage.lookupScopeAccount")
              : t("quotasPage.lookupScopeTeam")}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {/* Two-row grid: row 1 holds the labels, row 2 the controls. Pinning
              by row (instead of flex items-end over uneven label heights) keeps
              the Scope and Account labels on one baseline and the three controls
              flush on the next. */}
          <form
            onSubmit={lookup}
            className="grid grid-cols-[max-content_max-content_max-content] items-end justify-start gap-x-2 gap-y-1.5"
          >
            <Label htmlFor="quota-scope" className="self-start">
              {t("quotasPage.labelScope")}
            </Label>
            <Label className="self-start">
              {scope === "user" ? t("quotasPage.labelAccount") : t("quotasPage.labelTeam")}
            </Label>
            <span aria-hidden />
            <NativeSelect
              id="quota-scope"
              value={scope}
              onChange={(e) => changeScope(e.target.value as Scope)}
            >
              <NativeSelectOption value="user">{t("quotasPage.labelAccount")}</NativeSelectOption>
              <NativeSelectOption value="team">{t("quotasPage.labelTeam")}</NativeSelectOption>
            </NativeSelect>
            <OwnerPicker scope={scope} value={owner} onChange={setOwner} />
            <Button
              type="submit"
              variant="outline"
              disabled={busy || !owner}
              className="justify-self-start"
            >
              <Search />
              {busy ? t("quotasPage.loading") : t("quotasPage.lookUp")}
            </Button>
          </form>

          {error && <ErrorNotice message={error} />}

          {searched && !error && (
            <div className="space-y-4 border-t border-border pt-4">
              <div className="flex items-center gap-2 text-sm">
                <span className="text-muted-foreground">{t("quotasPage.currentCap")}</span>
                {result ? (
                  <>
                    <Badge variant="secondary" className="tabular-nums">
                      {t("quotasPage.tokensPerMonth", { count: formatCount(result.monthly_tokens) })}
                    </Badge>
                    <span className="text-xs text-muted-foreground">
                      {t("quotasPage.setAt", { time: formatDateTime(result.updated_at) })}
                    </span>
                  </>
                ) : (
                  <Badge variant="outline">{t("quotasPage.noCap")}</Badge>
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
                  <Label htmlFor="quota-limit">{t("quotasPage.monthlyTokens")}</Label>
                  <Input
                    id="quota-limit"
                    type="number"
                    min={1}
                    inputMode="numeric"
                    value={limit}
                    onChange={(e) => setLimit(e.target.value)}
                    placeholder={t("quotasPage.limitPlaceholder")}
                    className="w-56 tabular-nums"
                  />
                  <p className="text-xs text-muted-foreground">
                    {t("quotasPage.billableHint")}
                  </p>
                </div>
                <Button
                  type="submit"
                  disabled={busy || !Number.isFinite(parsedLimit) || parsedLimit <= 0}
                >
                  {result ? t("quotasPage.updateCap") : t("quotasPage.setCap")}
                </Button>
                {result && (
                  <ConfirmButton
                    title={t("quotasPage.clearTitle")}
                    description={
                      scope === "user"
                        ? t("quotasPage.clearDescriptionAccount")
                        : t("quotasPage.clearDescriptionTeam")
                    }
                    confirmLabel={t("quotasPage.clearCap")}
                    onConfirm={clear}
                    trigger={
                      <Button variant="ghost" disabled={busy}>
                        <Trash2 />
                        {t("quotasPage.clearCap")}
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
// so it stays selected while results for a new query stream in. Shared with
// the platform memories page, which picks an owner the same way.
export function OwnerPicker({
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
    ? t("quotasPage.searching")
    : searchError
      ? searchError
      : trimmed === ""
        ? value
          ? null
          : scope === "user"
            ? t("quotasPage.typeToSearchAccounts")
            : t("quotasPage.typeToSearchTeams")
        : results.length === 0
          ? scope === "user"
            ? t("quotasPage.noAccountMatches", { term: trimmed })
            : t("quotasPage.noTeamMatches", { term: trimmed })
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
                    (tm): Owner => ({
                      id: tm.id,
                      label: tm.name,
                      sublabel:
                        tm.members === undefined
                          ? undefined
                          : t("quotasPage.membersCount", { count: tm.members }),
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
        placeholder={scope === "user" ? t("quotasPage.searchAccounts") : t("quotasPage.searchTeams")}
        showClear
        // The trigger/clear addon's default py-1.5 (24px button + 12px padding)
        // makes it 36px — taller than the h-8 group, so the field hung 4px below
        // the scope select beside it. Strip that vertical padding and clip the
        // overflow so the control is exactly h-8 like its row-mates.
        className="w-80 overflow-hidden [&_[data-slot=input-group-addon]]:py-0"
      />
      <ComboboxContent>
        <ComboboxPrimitive.Status className="flex items-center gap-2 px-2 py-1.5 text-xs text-muted-foreground data-empty:hidden">
          {status}
        </ComboboxPrimitive.Status>
        <ComboboxEmpty>
          {trimmed !== "" && !isPending && results.length === 0 && !searchError
            ? t("quotasPage.tryDifferent")
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
