// CheckboxGroup renders a multi-select setting (redaction categories) as
// checkboxes; an empty selection means "all" and is shown as a hint.

import { Checkbox } from "@/components/ui/checkbox";

export function CheckboxGroup({
  options,
  selected,
  onChange,
  emptyHint = "Empty = all categories",
}: {
  options: { value: string; label: string }[];
  selected: string[];
  onChange: (selected: string[]) => void;
  emptyHint?: string;
}) {
  const toggle = (value: string) => {
    onChange(
      selected.includes(value)
        ? selected.filter((v) => v !== value)
        : [...selected, value]
    );
  };
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-x-4 gap-y-2">
        {options.map((o) => (
          <label
            key={o.value}
            className="flex cursor-pointer items-center gap-1.5 text-sm"
          >
            <Checkbox
              checked={selected.includes(o.value)}
              onCheckedChange={() => toggle(o.value)}
            />
            <span className="font-mono text-xs">{o.label}</span>
          </label>
        ))}
      </div>
      {selected.length === 0 && (
        <p className="text-xs text-muted-foreground">{emptyHint}</p>
      )}
    </div>
  );
}
