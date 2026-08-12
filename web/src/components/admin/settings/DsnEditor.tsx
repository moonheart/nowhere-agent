// DsnEditor edits the query_db business databases as a row table: each row is
// a name plus its DSN. Rows are added/removed; the value serializes back to
// "name=dsn,name=dsn,…". Names are validated as [a-z0-9_-].

import { Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

export type DsnRow = { name: string; dsn: string };

const NAME_RE = /^[a-z0-9_-]+$/;

export function DsnEditor({
  rows,
  onChange,
}: {
  rows: DsnRow[];
  onChange: (rows: DsnRow[]) => void;
}) {
  const patch = (i: number, field: keyof DsnRow, v: string) => {
    const next = rows.map((r, idx) => (idx === i ? { ...r, [field]: v } : r));
    onChange(next);
  };

  return (
    <div className="w-full space-y-2">
      {rows.map((r, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            value={r.name}
            onChange={(e) => patch(i, "name", e.target.value)}
            placeholder="name (erp, crm, …)"
            aria-label="database name"
            className={
              r.name !== "" && !NAME_RE.test(r.name)
                ? "w-36 border-destructive"
                : "w-36"
            }
          />
          <Input
            value={r.dsn}
            onChange={(e) => patch(i, "dsn", e.target.value)}
            placeholder="postgres://ro:secret@pg.internal:5432/erp"
            aria-label="database DSN"
            className="font-mono text-xs flex-1"
          />
          <Button
            variant="ghost"
            size="sm"
            onClick={() => onChange(rows.filter((_, idx) => idx !== i))}
            aria-label={`Remove ${r.name || "row"}`}
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      ))}
      <Button
        variant="outline"
        size="sm"
        onClick={() => onChange([...rows, { name: "", dsn: "" }])}
      >
        <Plus /> Add database
      </Button>
      {rows.some((r) => r.name !== "" && !NAME_RE.test(r.name)) && (
        <p className="text-xs text-destructive">
          Names must be lowercase letters, digits, _ or -.
        </p>
      )}
    </div>
  );
}
