import type { FC } from "react";
import { CheckCircle2, Circle, Loader2 } from "lucide-react";
import { usePlan, type PlanItem } from "@/lib/plan";

function Item({ item }: { item: PlanItem }) {
  const label =
    item.status === "in_progress" && item.activeForm ? item.activeForm : item.content;
  return (
    <li className="flex items-center gap-2 text-sm">
      {item.status === "completed" ? (
        <CheckCircle2 size={15} className="shrink-0 text-emerald-600" />
      ) : item.status === "in_progress" ? (
        <Loader2 size={15} className="shrink-0 animate-spin text-violet-600" />
      ) : (
        <Circle size={15} className="shrink-0 text-neutral-300" />
      )}
      <span
        className={
          item.status === "completed"
            ? "text-neutral-400 line-through"
            : item.status === "in_progress"
              ? "font-medium text-neutral-800"
              : "text-neutral-500"
        }
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
    <div className="border-b border-neutral-200 bg-neutral-50 px-6 py-3">
      <div className="mx-auto max-w-3xl">
        <div className="mb-1.5 flex items-center gap-2">
          <span className="text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Plan
          </span>
          <span className="text-xs text-neutral-400">
            {done}/{plan.items.length} completed
          </span>
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
