// PermissionMatrix edits the four permission_* settings as one decision
// matrix: rows are the tool risk levels, columns the allow/ask/deny verdicts.
// A row's selection is saved per key; the matrix makes the whole policy
// readable at a glance instead of four isolated inputs.

import { cn } from "@/lib/utils";

const DECISIONS = [
  { value: "allow", label: "allow", hint: "runs without approval" },
  { value: "ask", label: "ask", hint: "suspends for approval (headless = deny)" },
  { value: "deny", label: "deny", hint: "blocks the tool" },
];

export const PERMISSION_ROWS: { key: string; label: string }[] = [
  { key: "permission_read_only", label: "Read-only (query_db, recall_memory, …)" },
  { key: "permission_sandbox_write", label: "Sandbox write (file tools, plan_write, …)" },
  { key: "permission_network", label: "Network (http_request, MCP tools, web search)" },
  { key: "permission_external_write", label: "External write (memory writes)" },
];

export function PermissionMatrix({
  values,
  onChange,
}: {
  values: Record<string, string>;
  onChange: (key: string, value: string) => void;
}) {
  return (
    <div className="w-full overflow-x-auto">
      <table className="w-full min-w-105 border-separate border-spacing-1 text-sm">
        <thead>
          <tr>
            <th className="text-left font-medium text-muted-foreground">Risk class</th>
            {DECISIONS.map((d) => (
              <th key={d.value} className="font-medium text-muted-foreground">
                {d.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {PERMISSION_ROWS.map((row) => (
            <tr key={row.key}>
              <td className="py-1 pr-3">{row.label}</td>
              {DECISIONS.map((d) => (
                <td key={d.value} className="p-0.5">
                  <button
                    type="button"
                    aria-label={`${row.label}: ${d.label}`}
                    title={d.hint}
                    onClick={() => onChange(row.key, d.value)}
                    className={cn(
                      "w-full rounded-md border px-3 py-1.5 text-center text-xs font-medium transition-colors",
                      values[row.key] === d.value
                        ? "border-primary bg-primary/10 text-foreground"
                        : "border-input text-muted-foreground hover:bg-muted"
                    )}
                  >
                    {d.label}
                  </button>
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
