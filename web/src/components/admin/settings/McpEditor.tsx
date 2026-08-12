// McpEditor edits the mcp_servers JSON as a server table: each row is a
// server (name, URL, timeout in seconds) with an expandable headers section
// (key/value pairs). Rows serialize back to the MCP_SERVERS JSON the backend
// validates. The stored value is a secret — the editor starts from an empty
// list and the current value is never loaded back into the form.

import { useState } from "react";
import { ChevronDown, ChevronRight, Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

export type McpHeader = { key: string; value: string };

export type McpServerRow = {
  name: string;
  url: string;
  timeoutSeconds: number;
  headers: McpHeader[];
};

// toJson serializes the rows to the MCP_SERVERS wire JSON (timeout "Ns").
export function mcpRowsToJson(rows: McpServerRow[]): string {
  return JSON.stringify(
    rows
      .filter((r) => r.name !== "" || r.url !== "")
      .map((r) => ({
        name: r.name,
        url: r.url,
        ...(r.headers.some((h) => h.key !== "") ? {
          headers: Object.fromEntries(
            r.headers.filter((h) => h.key !== "").map((h) => [h.key, h.value])
          ),
        } : {}),
        ...(r.timeoutSeconds > 0 ? { timeout: `${r.timeoutSeconds}s` } : {}),
      })),
    null,
    2
  );
}

export function McpEditor({
  rows,
  onChange,
}: {
  rows: McpServerRow[];
  onChange: (rows: McpServerRow[]) => void;
}) {
  const [open, setOpen] = useState<number | null>(null);

  const patch = (i: number, field: keyof McpServerRow, v: unknown) => {
    onChange(rows.map((r, idx) => (idx === i ? { ...r, [field]: v } : r)));
  };
  const patchHeader = (i: number, j: number, field: keyof McpHeader, v: string) => {
    onChange(
      rows.map((r, idx) =>
        idx === i
          ? { ...r, headers: r.headers.map((h, hj) => (hj === j ? { ...h, [field]: v } : h)) }
          : r
      )
    );
  };

  return (
    <div className="w-full space-y-3">
      {rows.map((r, i) => (
        <div key={i} className="rounded-lg border border-input p-3 space-y-2">
          <div className="flex items-center gap-2">
            <Input
              value={r.name}
              onChange={(e) => patch(i, "name", e.target.value)}
              placeholder="server name (erp, kb, …)"
              aria-label="server name"
              className="w-40 font-mono text-xs"
            />
            <Input
              value={r.url}
              onChange={(e) => patch(i, "url", e.target.value)}
              placeholder="https://mcp.internal/erp/mcp"
              aria-label="server URL"
              className="flex-1 font-mono text-xs"
            />
            <Input
              type="number"
              min={0}
              step={1}
              value={r.timeoutSeconds || ""}
              onChange={(e) =>
                patch(i, "timeoutSeconds", Math.max(0, Number(e.target.value) || 0))
              }
              placeholder="timeout s"
              aria-label="timeout seconds"
              className="w-24"
            />
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onChange(rows.filter((_, idx) => idx !== i))}
              aria-label={`Remove ${r.name || "server"}`}
            >
              <Trash2 className="size-4" />
            </Button>
          </div>
          <button
            type="button"
            onClick={() => setOpen(open === i ? null : i)}
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            {open === i ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
            Headers{` (${r.headers.filter((h) => h.key !== "").length})`}
          </button>
          {open === i && (
            <div className={cn("space-y-1.5 pl-4")}>
              {r.headers.map((h, j) => (
                <div key={j} className="flex items-center gap-2">
                  <Input
                    value={h.key}
                    onChange={(e) => patchHeader(i, j, "key", e.target.value)}
                    placeholder="Header name (Authorization)"
                    aria-label="header name"
                    className="w-48 font-mono text-xs"
                  />
                  <Input
                    type="password"
                    value={h.value}
                    onChange={(e) => patchHeader(i, j, "value", e.target.value)}
                    placeholder="value"
                    aria-label="header value"
                    className="flex-1 font-mono text-xs"
                  />
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() =>
                      onChange(
                        rows.map((row, idx) =>
                          idx === i
                            ? { ...row, headers: row.headers.filter((_, hj) => hj !== j) }
                            : row
                        )
                      )
                    }
                    aria-label="Remove header"
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </div>
              ))}
              <Button
                variant="outline"
                size="sm"
                onClick={() =>
                  onChange(
                    rows.map((row, idx) =>
                      idx === i ? { ...row, headers: [...row.headers, { key: "", value: "" }] } : row
                    )
                  )
                }
              >
                <Plus /> Add header
              </Button>
            </div>
          )}
        </div>
      ))}
      <Button
        variant="outline"
        size="sm"
        onClick={() =>
          onChange([...rows, { name: "", url: "", timeoutSeconds: 0, headers: [] }])
        }
      >
        <Plus /> Add server
      </Button>
    </div>
  );
}
