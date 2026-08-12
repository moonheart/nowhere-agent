// SwitchRow renders a boolean setting as a labelled toggle switch.

import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";

export function SwitchRow({
  checked,
  onChange,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <div className="flex items-center gap-2">
      <Switch checked={checked} onCheckedChange={onChange} />
      <Label className="text-sm font-normal text-muted-foreground">
        {checked ? "enabled" : "disabled"}
      </Label>
    </div>
  );
}
