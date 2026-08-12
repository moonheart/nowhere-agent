// NumberWithUnit edits a duration-typed setting (stored as seconds, or as
// days for the memory purge window) as a number plus a unit dropdown. The
// display picks the largest unit that divides the current value.

import { useMemo, useState } from "react";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { Input } from "@/components/ui/input";

export type Unit = { label: string; seconds: number };

export const SECONDS_UNITS: Unit[] = [
  { label: "seconds", seconds: 1 },
  { label: "minutes", seconds: 60 },
  { label: "hours", seconds: 3600 },
];

export const DAY_UNITS: Unit[] = [
  { label: "days", seconds: 86400 },
  { label: "weeks", seconds: 604800 },
];

// pickUnit finds the largest unit dividing value (falling back to the
// smallest), so "120" renders as "2 minutes", not "120 seconds".
function pickUnit(units: Unit[], value: number): Unit {
  for (let i = units.length - 1; i >= 0; i--) {
    if (value > 0 && value % units[i].seconds === 0) return units[i];
  }
  return units[0];
}

export function NumberWithUnit({
  value,
  onChange,
  units,
  min = 0,
  disabledHint,
}: {
  value: number;
  onChange: (seconds: number) => void;
  units: Unit[];
  min?: number;
  disabledHint?: string;
}) {
  const [unit, setUnit] = useState<Unit>(() => pickUnit(units, value));
  const [draft, setDraft] = useState<string | null>(null);

  const shown = useMemo(() => {
    if (draft !== null) return draft;
    return String(Math.round(value / unit.seconds));
  }, [draft, value, unit.seconds]);

  const commit = (raw: string) => {
    const n = Number(raw);
    if (Number.isFinite(n) && n >= 0) {
      onChange(Math.round(n * unit.seconds));
    }
  };

  return (
    <div className="flex w-full gap-2">
      <Input
        type="number"
        min={min}
        step="1"
        value={shown}
        onChange={(e) => {
          setDraft(e.target.value);
          commit(e.target.value);
        }}
        onBlur={() => setDraft(null)}
        aria-label="duration"
      />
      <NativeSelect
        value={unit.label}
        onChange={(e) => {
          const u = units.find((x) => x.label === e.target.value) ?? units[0];
          setUnit(u);
          const n = Number(shown);
          if (Number.isFinite(n)) onChange(Math.round(n * u.seconds));
        }}
        className="w-28"
      >
        {units.map((u) => (
          <NativeSelectOption key={u.label} value={u.label}>
            {u.label}
          </NativeSelectOption>
        ))}
      </NativeSelect>
      {disabledHint && <p className="text-xs text-muted-foreground">{disabledHint}</p>}
    </div>
  );
}
