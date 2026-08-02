import type { FC } from "react";
import { CheckCircle2, Circle, Loader2 } from "lucide-react";
import { usePlan, type PlanItem } from "@/lib/plan";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

function Item({ item }: { item: PlanItem }) {
  const label =
    item.status === "in_progress" && item.activeForm ? item.activeForm : item.content;
  return (
    <li className="flex items-center gap-2 text-sm">
      {item.status === "completed" ? (
        <CheckCircle2 className="size-4 shrink-0 text-emerald-600" />
      ) : item.status === "in_progress" ? (
        <Loader2 className="size-4 shrink-0 animate-spin text-primary" />
      ) : (
        <Circle className="size-4 shrink-0 text-muted-foreground/40" />
      )}
      <span
        className={cn(
          item.status === "completed"
            ? "text-muted-foreground line-through"
            : item.status === "in_progress"
              ? "font-medium text-foreground"
              : "text-muted-foreground",
        )}
      >
        {label}
      </span>
    </li>
  );
}

// PlanPanel is the top, session-level view of the agent's working plan
// (capability-gap O1). It reads the module-level plan store, which is fed live
// by data-session-state frames and restored on reload from the history echo, so
// the panel tracks the plan in real time and survives a refresh. Hidden when no
// plan has been recorded for the session.
export const PlanPanel: FC = () => {
  const plan = usePlan();
  if (!plan || plan.items.length === 0) return null;

  const done = plan.items.filter((i) => i.status === "completed").length;
  return (
    <div className="border-b border-border bg-muted/50 px-6 py-3">
      <div className="mx-auto max-w-3xl">
        <div className="mb-1.5 flex items-center gap-2">
          <span className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">
            Plan
          </span>
          <Badge variant="secondary">
            {done}/{plan.items.length} completed
          </Badge>
        </div>
        <ul className="flex flex-col gap-1">
          {plan.items.map((item, i) => (
            <Item key={i} item={item} />
          ))}
        </ul>
      </div>
    </div>
  );
};
