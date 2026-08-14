// Platform audit trail viewer (enterprise-readiness P0-1). Read-only: the
// trail is append-only by design, so this page offers filters and pagination
// but no mutation. It sits behind the platform-admin guard, alongside Users /
// Teams / Usage, and names actors and targets — which is exactly why the server
// restricts it to administrators in the first place.

import { useState } from "react";
import { Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  AUDIT_ACTIONS,
  listAudit,
  type AuditEntry,
} from "@/lib/admin";
import { t } from "@/lib/i18n";
import {
  AsyncSection,
  formatDateTime,
  PageHeader,
  useAsync,
} from "@/components/admin/common";

const PAGE_SIZE = 50;

type Filters = { action: string; actor: string; from: string; to: string };

const EMPTY: Filters = { action: "", actor: "", from: "", to: "" };

export function PlatformAuditPage() {
  // `draft` is what the filter inputs show; `applied` is what the query uses.
  // Keeping them apart means typing does not fire a request per keystroke — the
  // list only refetches when the operator submits, which also resets pagination.
  const [draft, setDraft] = useState<Filters>(EMPTY);
  const [applied, setApplied] = useState<Filters>(EMPTY);
  const [offset, setOffset] = useState(0);

  const state = useAsync(
    () =>
      listAudit({
        action: applied.action || undefined,
        actor: applied.actor || undefined,
        from: applied.from || undefined,
        to: applied.to || undefined,
        limit: PAGE_SIZE,
        offset,
      }),
    [applied, offset],
  );

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    setOffset(0);
    setApplied({
      action: draft.action,
      actor: draft.actor.trim(),
      from: draft.from,
      to: draft.to,
    });
  };

  return (
    <>
      <PageHeader
        title={t("auditPage.title")}
        description={t("auditPage.description")}
      />

      <form onSubmit={submit} className="flex flex-wrap items-end gap-2">
        <div className="space-y-1.5">
          <Label htmlFor="audit-action">{t("auditPage.labelAction")}</Label>
          <NativeSelect
            id="audit-action"
            value={draft.action}
            onChange={(e) => setDraft({ ...draft, action: e.target.value })}
          >
            <NativeSelectOption value="">{t("auditPage.allActions")}</NativeSelectOption>
            {AUDIT_ACTIONS.map((a) => (
              <NativeSelectOption key={a} value={a}>
                {a}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="audit-actor">{t("auditPage.labelActor")}</Label>
          <Input
            id="audit-actor"
            value={draft.actor}
            onChange={(e) => setDraft({ ...draft, actor: e.target.value })}
            placeholder={t("auditPage.actorPlaceholder")}
            className="w-56 font-mono text-sm"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="audit-from">{t("auditPage.labelFrom")}</Label>
          <Input
            id="audit-from"
            type="date"
            value={draft.from}
            onChange={(e) => setDraft({ ...draft, from: e.target.value })}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="audit-to">{t("auditPage.labelTo")}</Label>
          <Input
            id="audit-to"
            type="date"
            value={draft.to}
            onChange={(e) => setDraft({ ...draft, to: e.target.value })}
          />
        </div>
        <Button type="submit" variant="outline" size="sm">
          <Search />
          {t("auditPage.filter")}
        </Button>
      </form>

      <AsyncSection state={state} loadingLabel={t("auditPage.loading")}>
        {(data) => (
          <div className="space-y-4">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-44">{t("auditPage.colTime")}</TableHead>
                  <TableHead>{t("auditPage.colActor")}</TableHead>
                  <TableHead className="w-56">{t("auditPage.colAction")}</TableHead>
                  <TableHead className="w-24">{t("auditPage.colOutcome")}</TableHead>
                  <TableHead>{t("auditPage.colTarget")}</TableHead>
                  <TableHead className="w-32">{t("auditPage.colIP")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.entries.map((en) => (
                  <AuditRow key={en.id} entry={en} />
                ))}
              </TableBody>
            </Table>

            <div className="flex items-center justify-between text-sm text-muted-foreground">
              <span>
                {data.total === 0
                  ? t("auditPage.noEvents")
                  : t("auditPage.range", {
                      from: data.offset + 1,
                      to: Math.min(data.offset + PAGE_SIZE, data.total),
                      total: data.total,
                    })}
              </span>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={data.offset === 0}
                  onClick={() => setOffset(Math.max(0, data.offset - PAGE_SIZE))}
                >
                  {t("auditPage.previous")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={data.offset + PAGE_SIZE >= data.total}
                  onClick={() => setOffset(data.offset + PAGE_SIZE)}
                >
                  {t("auditPage.next")}
                </Button>
              </div>
            </div>
          </div>
        )}
      </AsyncSection>
    </>
  );
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  return (
    <TableRow>
      <TableCell className="text-sm whitespace-nowrap text-muted-foreground">
        {formatDateTime(entry.created_at)}
      </TableCell>
      <TableCell>
        <div className="text-sm">{entry.actor_email || "—"}</div>
        {entry.actor_id && (
          <div className="font-mono text-xs text-muted-foreground">
            {entry.actor_id.slice(0, 8)}
          </div>
        )}
      </TableCell>
      <TableCell className="font-mono text-xs">{entry.action}</TableCell>
      <TableCell>
        <OutcomeBadge outcome={entry.outcome} />
      </TableCell>
      <TableCell className="text-sm text-muted-foreground">
        {entry.target_type
          ? `${entry.target_type}${entry.target_id ? ` ${entry.target_id.slice(0, 8)}` : ""}`
          : "—"}
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">
        {entry.ip || "—"}
      </TableCell>
    </TableRow>
  );
}

function OutcomeBadge({ outcome }: { outcome: string }) {
  if (outcome === "success") {
    return <Badge variant="secondary">{t("auditPage.outcomeSuccess")}</Badge>;
  }
  return <Badge variant="destructive">{outcome}</Badge>;
}
