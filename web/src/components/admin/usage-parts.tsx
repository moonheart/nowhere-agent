// Pieces shared by the three usage views: the period selector, the daily trend
// chart, and the notice that team figures are an approximation.

import { useState } from "react";
import { Info } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart";
import { Bar, BarChart, CartesianGrid, XAxis } from "recharts";
import type { DateRange, UsageRow } from "@/lib/admin";

const PERIODS = [
  { label: "Last 7 days", days: 7 },
  { label: "Last 30 days", days: 30 },
  { label: "Last 90 days", days: 90 },
  { label: "All time", days: 0 },
] as const;

// useDateRange keeps the selected period as concrete dates, so the value sent
// to the server does not drift while the page is open.
export function useDateRange(defaultDays = 30) {
  const [days, setDays] = useState(defaultDays);
  const [range, setRangeState] = useState<DateRange>(() => rangeForDays(defaultDays));

  const setRange = (d: number) => {
    setDays(d);
    setRangeState(rangeForDays(d));
  };
  return { range, days, setRange, setRangeState };
}

function rangeForDays(days: number): DateRange {
  if (days <= 0) return {};
  const from = new Date();
  from.setDate(from.getDate() - days);
  return { from: from.toISOString().slice(0, 10) };
}

export function DateRangePicker({
  range,
  onChange,
}: {
  range: DateRange;
  onChange: (days: number) => void;
}) {
  const current = range.from
    ? PERIODS.find((p) => p.days > 0 && rangeForDays(p.days).from === range.from)?.days
    : 0;
  return (
    <NativeSelect
      size="sm"
      value={String(current ?? 30)}
      onChange={(e) => onChange(Number(e.target.value))}
      aria-label="Reporting period"
    >
      {PERIODS.map((p) => (
        <NativeSelectOption key={p.days} value={String(p.days)}>
          {p.label}
        </NativeSelectOption>
      ))}
    </NativeSelect>
  );
}

const chartConfig = {
  input: { label: "Input", color: "var(--chart-1)" },
  output: { label: "Output", color: "var(--chart-2)" },
} satisfies ChartConfig;

// UsageTrend charts input and output per day. Cache counters are left out: they
// are priced differently and stacking them here would read as one total.
export function UsageTrend({ rows }: { rows: UsageRow[] }) {
  if (rows.length === 0) {
    return (
      <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
        No runs in this period.
      </p>
    );
  }
  const data = rows.map((r) => ({
    day: r.id,
    input: r.tokens.input,
    output: r.tokens.output,
  }));

  return (
    <ChartContainer config={chartConfig} className="h-56 w-full">
      <BarChart data={data} accessibilityLayer>
        <CartesianGrid vertical={false} />
        <XAxis
          dataKey="day"
          tickLine={false}
          axisLine={false}
          tickMargin={8}
          tickFormatter={(v: string) => v.slice(5)}
        />
        <ChartTooltip content={<ChartTooltipContent />} />
        <Bar dataKey="input" stackId="a" fill="var(--color-input)" radius={[0, 0, 2, 2]} />
        <Bar dataKey="output" stackId="a" fill="var(--color-output)" radius={[2, 2, 0, 0]} />
      </BarChart>
    </ChartContainer>
  );
}

// ApproximationNotice surfaces the server's caveat about team attribution.
// Team figures sum their members' usage, so they are not a partition — showing
// the numbers without this would misrepresent them.
export function ApproximationNotice({ note }: { note?: string }) {
  if (!note) return null;
  return (
    <Alert>
      <Info />
      <AlertTitle>These team figures overlap</AlertTitle>
      <AlertDescription>{note}</AlertDescription>
    </Alert>
  );
}
