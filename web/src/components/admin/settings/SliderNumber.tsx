// SliderNumber pairs a slider with a number input for a bounded numeric
// setting (LLM temperature: 0–2, step 0.1). A "use provider default" toggle
// covers the unset (negative) value, which is outside the slider range.

import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Slider } from "@/components/ui/slider";

export function SliderNumber({
  value,
  onChange,
  min = 0,
  max = 2,
  step = 0.1,
  unsetLabel = "provider default",
}: {
  value: number; // negative = unset
  onChange: (value: number) => void;
  min?: number;
  max?: number;
  step?: number;
  unsetLabel?: string;
}) {
  const unset = value < min;
  const bounded = Math.min(Math.max(value, min), max);

  return (
    <div className="w-full space-y-3">
      <div className="flex items-center gap-3">
        <Slider
          value={[bounded]}
          min={min}
          max={max}
          step={step}
          disabled={unset}
          onValueChange={(v) => {
            const n = Array.isArray(v) ? v[0] : v;
            if (typeof n === "number" && Number.isFinite(n)) {
              onChange(Math.round(n * 100) / 100);
            }
          }}
          className="flex-1"
        />
        <Input
          type="number"
          min={min}
          max={max}
          step={step}
          value={unset ? "" : String(bounded)}
          disabled={unset}
          onChange={(e) => {
            const n = Number(e.target.value);
            if (Number.isFinite(n) && n >= min && n <= max) {
              onChange(Math.round(n * 100) / 100);
            }
          }}
          className="w-20"
          aria-label="value"
        />
      </div>
      <label className="flex cursor-pointer items-center gap-2 text-sm text-muted-foreground">
        <Checkbox
          checked={unset}
          onCheckedChange={(c) => onChange(c ? -1 : min)}
        />
        <span>{unsetLabel}</span>
      </label>
    </div>
  );
}
