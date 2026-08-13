// Platform runtime settings (no-restart configuration): operator knobs that
// used to require editing env and restarting the gateway. The page is split
// into tabs, and every key gets a control matched to its shape: tag inputs
// for lists, a decision matrix for the permission policy, a row table for
// query_db databases, a server table for MCP, switches for booleans,
// segmented buttons for enums, sliders for bounded numbers, and unit-aware
// inputs for durations. Saving applies immediately on the next use; saving
// empty restores the environment default. Secret keys (webhook signing
// secret, MCP servers) are never echoed back. Changes are audited.

import { useState } from "react";
import { Save } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";
import { api } from "@/lib/api";
import { AsyncSection, ErrorNotice, PageHeader, useAsync } from "@/components/admin/common";
import { TagInput } from "@/components/admin/settings/TagInput";
import {
  DAY_UNITS,
  NumberWithUnit,
  SECONDS_UNITS,
} from "@/components/admin/settings/NumberWithUnit";
import { Segmented } from "@/components/admin/settings/Segmented";
import { SliderNumber } from "@/components/admin/settings/SliderNumber";
import { SwitchRow } from "@/components/admin/settings/SwitchRow";
import { CheckboxGroup } from "@/components/admin/settings/CheckboxGroup";
import {
  PermissionMatrix,
  PERMISSION_ROWS,
} from "@/components/admin/settings/PermissionMatrix";
import { DsnEditor, type DsnRow } from "@/components/admin/settings/DsnEditor";
import {
  McpEditor,
  mcpRowsToJson,
  type McpServerRow,
} from "@/components/admin/settings/McpEditor";
import { SecretField } from "@/components/admin/settings/SecretField";

type SettingKind = "string" | "int" | "float" | "bool";

type SettingEntry = {
  key: string;
  group: string;
  kind: SettingKind;
  value: string | number | boolean | null;
  secret: boolean;
  description: string;
};

// One local draft per shape family; unset drafts fall back to the parsed
// current value so controls open showing what is actually in effect.
type Draft =
  | string
  | number
  | boolean
  | string[]
  | DsnRow[]
  | McpServerRow[];

const GROUPS: { id: string; label: string }[] = [
  { id: "tools", label: "Tools" },
  { id: "webhooks", label: "Webhooks" },
  { id: "llm", label: "LLM / model" },
  { id: "sandbox", label: "Sandbox" },
  { id: "permissions", label: "Permissions" },
  { id: "redaction", label: "Redaction" },
  { id: "subagents", label: "Subagents" },
  { id: "background", label: "Background tasks" },
  { id: "http", label: "HTTP / gateway" },
  { id: "auth", label: "Auth / SSO" },
  { id: "integrations", label: "Integrations" },
];

// Keys whose value is a comma-separated list.
const LIST_KEYS = new Set([
  "http_tool_allowlist",
  "webhook_ssrf_allowlist",
  "redact_categories",
]);

// Keys whose value is a duration in seconds (unit-aware input).
const SECONDS_KEYS = new Set([
  "http_tool_timeout",
  "query_db_timeout",
  "webhook_timeout",
  "llm_stream_idle_timeout",
  "dreaming_interval",
  "schedule_scan_interval",
  "phone_sms_timeout",
]);

// Keys whose value is a duration in days.
const DAYS_KEYS = new Set(["dreaming_purge_after"]);

// Keys edited as segmented enum buttons.
const ENUM_KEYS: Record<string, { value: string; label: string }[]> = {
  llm_system_lang: [
    { value: "en", label: "English" },
    { value: "zh", label: "中文" },
  ],
  sandbox_network: [
    { value: "deny", label: "deny" },
    { value: "open", label: "open" },
    { value: "allowlist", label: "allowlist" },
  ],
  redact_strategy: [
    { value: "redact", label: "redact" },
    { value: "mask", label: "mask" },
  ],
};

const REDACT_CATEGORIES = [
  "email",
  "credit_card",
  "ipv4",
  "bearer",
  "basic_auth",
  "api_key",
  "private_key",
  "secret_value",
];

const listSettings = () =>
  api<{ settings: SettingEntry[] }>("/api/admin/settings");

const putSetting = (key: string, value: string | number | boolean | null) =>
  api<void>(`/api/admin/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    body: { value },
  });

const splitList = (s: string | number | boolean | null): string[] =>
  typeof s === "string" && s !== ""
    ? s.split(",").map((x) => x.trim()).filter(Boolean)
    : [];

function parseDsnRows(s: string | number | boolean | null): DsnRow[] {
  return splitList(typeof s === "string" ? s : "").map((entry) => {
    const eq = entry.indexOf("=");
    return eq > 0
      ? { name: entry.slice(0, eq).trim(), dsn: entry.slice(eq + 1).trim() }
      : { name: "", dsn: entry };
  });
}

function displayValue(s: SettingEntry): string {
  if (s.secret) {
    return s.value === null ? "(not set)" : "(set — hidden)";
  }
  if (s.value === null) return "(not set)";
  if (typeof s.value === "boolean") return s.value ? "true" : "false";
  return String(s.value);
}

export function PlatformSettingsPage() {
  const state = useAsync(listSettings, []);
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [mcpDraft, setMcpDraft] = useState<McpServerRow[] | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  // notice marks a Save click that had nothing to change ("Nothing to change.").
  const [notice, setNotice] = useState<string | null>(null);

  const setDraft = (key: string, v: Draft) =>
    setDrafts((d) => ({ ...d, [key]: v }));

  // wireValue serializes a draft to the backend's wire form for a key.
  const wireValue = (s: SettingEntry, d: Draft): string | number | boolean | null => {
    switch (s.key) {
      case "http_tool_allowlist":
      case "webhook_ssrf_allowlist":
      case "redact_categories":
        return (d as string[]).join(", ");
      case "query_db_dsns": {
        const rows = (d as DsnRow[]).filter((r) => r.name !== "" || r.dsn !== "");
        if (rows.length === 0) return null;
        return rows.map((r) => `${r.name}=${r.dsn}`).join(", ");
      }
      case "mcp_servers": {
        // An empty table must CLEAR the override (back to the env default),
        // not persist "[]": the runtime's raw() prefers any stored row over
        // the env default, so a persisted "[]" would mask MCP_SERVERS and
        // applyMCP would tear down every env-configured server. Same
        // null-on-empty convention as query_db_dsns / webhook_signing_secret.
        const json = mcpRowsToJson(d as McpServerRow[]);
        return json === "[]" ? null : json;
      }
      case "webhook_signing_secret":
        return (d as string) === "" ? null : (d as string);
      default:
        return d as string | number | boolean | null;
    }
  };

  // save submits one key; empty drafts clear the override (back to default).
  const save = async (s: SettingEntry, d?: Draft) => {
    // Secret rows never echo their value back, so an untouched Save would PUT
    // null and wipe the configured override. Exactly these two can tell "no
    // local edit" apart from a deliberate clear: mcpDraft stays null until the
    // table is touched, and the secret field only enters drafts once typed.
    if (
      (s.key === "mcp_servers" && mcpDraft === null) ||
      (s.key === "webhook_signing_secret" && drafts[s.key] === undefined)
    ) {
      setSaved(null);
      setNotice(s.key);
      return;
    }
    // mcpDraft lives outside drafts (secret rows are never echoed back), so a
    // plain save must read it or the edited rows are silently dropped.
    const draft: Draft =
      s.key === "mcp_servers"
        ? (mcpDraft ?? [])
        : d !== undefined
          ? d
          : (drafts[s.key] ?? s.value ?? "");
    setBusy(s.key);
    setError(null);
    setSaved(null);
    setNotice(null);
    try {
      const wire = wireValue(s, draft);
      if (s.secret && typeof wire === "string" && wire === "") {
        await putSetting(s.key, null);
      } else {
        await putSetting(s.key, wire);
      }
      setSaved(s.key);
      setDrafts((dd) => {
        const next = { ...dd };
        delete next[s.key];
        return next;
      });
      if (s.key === "mcp_servers") setMcpDraft(null);
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  // savePermission applies one matrix cell immediately (click = save).
  const savePermission = async (key: string, value: string) => {
    setBusy(key);
    setError(null);
    setSaved(null);
    try {
      await putSetting(key, value);
      setSaved(key);
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const byGroup = (group: string) =>
    (state.data?.settings ?? []).filter((s) => s.group === group);

  const renderEditor = (s: SettingEntry) => {
    const current = s.value;
    if (s.key === "query_db_dsns") {
      const rows = (drafts[s.key] as DsnRow[] | undefined) ?? parseDsnRows(current);
      return (
        <DsnEditor
          rows={rows}
          onChange={(v) => setDraft(s.key, v)}
        />
      );
    }
    if (s.key === "mcp_servers") {
      const rows = mcpDraft ?? [];
      return (
        <>
          <McpEditor rows={rows} onChange={setMcpDraft} />
          <p className="text-xs text-muted-foreground">
            Current value is never loaded back (secret). A table with no servers
            clears the setting (back to the env default). Timeout in seconds
            (empty = client default).
          </p>
        </>
      );
    }
    if (s.key === "webhook_signing_secret") {
      const v = (drafts[s.key] as string | undefined) ?? "";
      return (
        <SecretField
          isSet={s.value !== null}
          value={v}
          onChange={(x) => setDraft(s.key, x)}
        />
      );
    }
    if (s.key === "llm_temperature") {
      const v = (drafts[s.key] as number | undefined) ?? Number(current ?? -1);
      return (
        <SliderNumber
          value={v}
          onChange={(x) => setDraft(s.key, x)}
          unsetLabel="Use provider default"
        />
      );
    }
    if (SECONDS_KEYS.has(s.key) || DAYS_KEYS.has(s.key)) {
      const v = (drafts[s.key] as number | undefined) ?? Number(current ?? 0);
      return (
        <NumberWithUnit
          value={v}
          onChange={(x) => setDraft(s.key, x)}
          units={DAYS_KEYS.has(s.key) ? DAY_UNITS : SECONDS_UNITS}
          disabledHint={
            s.key === "llm_stream_idle_timeout"
              ? "0 disables the stall guard"
              : undefined
          }
        />
      );
    }
    if (s.key === "webhook_retries") {
      const v = (drafts[s.key] as number | undefined) ?? Number(current ?? 0);
      return (
        <Input
          type="number"
          min={0}
          max={10}
          step={1}
          value={String(v)}
          onChange={(e) => setDraft(s.key, Number(e.target.value) || 0)}
        />
      );
    }
    if (ENUM_KEYS[s.key]) {
      const v = (drafts[s.key] as string | undefined) ?? String(current ?? "");
      return (
        <Segmented
          options={ENUM_KEYS[s.key]}
          value={v}
          onChange={(x) => setDraft(s.key, x)}
        />
      );
    }
    if (s.key === "phone_sms_url") {
      const v = (drafts[s.key] as string | undefined) ?? String(current ?? "");
      const channel = v === "log://" ? "log" : v === "" ? "custom" : "custom";
      return (
        <div className="flex w-full gap-2">
          <NativeSelect
            value={channel}
            onChange={(e) => {
              if (e.target.value === "log") setDraft(s.key, "log://");
              else if (e.target.value === "custom" && (drafts[s.key] as string | undefined) === "log://") {
                setDraft(s.key, "");
              }
            }}
            className="w-44"
          >
            <NativeSelectOption value="custom">Custom gateway URL</NativeSelectOption>
            <NativeSelectOption value="log">log:// (dev: print codes)</NativeSelectOption>
          </NativeSelect>
          {channel === "custom" && (
            <Input
              type="url"
              value={v === "log://" ? "" : v}
              placeholder="https://sms-gateway.internal/send"
              onChange={(e) => setDraft(s.key, e.target.value)}
              className="flex-1"
            />
          )}
        </div>
      );
    }
    if (s.key === "redact_categories") {
      const v = (drafts[s.key] as string[] | undefined) ?? splitList(current);
      return (
        <CheckboxGroup
          options={REDACT_CATEGORIES.map((c) => ({ value: c, label: c }))}
          selected={v}
          onChange={(x) => setDraft(s.key, x)}
          emptyHint="Empty = all categories"
        />
      );
    }
    if (s.kind === "bool") {
      const v = (drafts[s.key] as boolean | undefined) ?? (current === true);
      return (
        <SwitchRow
          checked={v}
          onChange={(x) => setDraft(s.key, x)}
        />
      );
    }
    if (LIST_KEYS.has(s.key)) {
      const v = (drafts[s.key] as string[] | undefined) ?? splitList(current);
      return (
        <TagInput
          values={v}
          onChange={(x) => setDraft(s.key, x)}
          placeholder={
            s.key === "redact_categories"
              ? "type a category and press Enter (empty = all)"
              : "type an entry and press Enter"
          }
        />
      );
    }
    // Plain number/string.
    const v = (drafts[s.key] as string | number | undefined) ?? current ?? "";
    return (
      <Input
        type={s.kind === "int" || s.kind === "float" ? "number" : "text"}
        step={s.kind === "float" ? "any" : undefined}
        value={String(v)}
        onChange={(e) =>
          setDraft(
            s.key,
            s.kind === "int" || s.kind === "float"
              ? Number(e.target.value) || 0
              : e.target.value
          )
        }
      />
    );
  };

  const settingById = (key: string): SettingEntry | undefined =>
    (state.data?.settings ?? []).find((s) => s.key === key);

  return (
    <>
      <PageHeader
        title="Platform settings"
        description="Runtime configuration — changes apply immediately, no restart. Saving an empty value restores the environment default."
      />
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading settings">
        {() => {
          const permValues = Object.fromEntries(
            PERMISSION_ROWS.map((r) => [
              r.key,
              String(settingById(r.key)?.value ?? ""),
            ])
          );
          return (
            <Tabs defaultValue="tools">
              <TabsList variant="line">
                {GROUPS.map((g) => (
                  <TabsTrigger key={g.id} value={g.id}>
                    {g.label}
                  </TabsTrigger>
                ))}
              </TabsList>
              {GROUPS.map((g) => (
                <TabsContent key={g.id} value={g.id}>
                  <div className="grid gap-4">
                    {g.id === "permissions" ? (
                      <Card>
                        <CardHeader>
                          <CardTitle className="text-sm">
                            Execution-permission policy
                          </CardTitle>
                          <CardDescription>
                            One matrix for every tool risk class — click a
                            verdict to apply it immediately. "ask" suspends the
                            run for approval (headless runs treat it as deny).
                          </CardDescription>
                        </CardHeader>
                        <CardContent>
                          <PermissionMatrix
                            values={permValues}
                            onChange={savePermission}
                          />
                        </CardContent>
                      </Card>
                    ) : (
                      byGroup(g.id).map((s) => (
                        <Card key={s.key}>
                          <CardHeader>
                            <CardTitle className="font-mono text-sm">
                              {s.key}
                            </CardTitle>
                            <CardDescription>{s.description}</CardDescription>
                          </CardHeader>
                          <CardContent className="space-y-3">
                            <div className="flex items-end gap-3">
                              <div className="flex-1 space-y-1.5">
                                <Label htmlFor={`setting-${s.key}`}>
                                  Current value:{" "}
                                  <span className="font-mono text-muted-foreground">
                                    {displayValue(s)}
                                  </span>
                                </Label>
                                {renderEditor(s)}
                              </div>
                              <Button
                                disabled={busy === s.key}
                                onClick={() => save(s)}
                              >
                                {busy === s.key ? "Saving…" : <><Save /> Save</>}
                              </Button>
                            </div>
                            {saved === s.key && (
                              <p className="text-sm text-muted-foreground">
                                Applied — no restart needed.
                              </p>
                            )}
                            {notice === s.key && (
                              <p className="text-sm text-muted-foreground">
                                Nothing to change.
                              </p>
                            )}
                          </CardContent>
                        </Card>
                      ))
                    )}
                  </div>
                </TabsContent>
              ))}
            </Tabs>
          );
        }}
      </AsyncSection>
    </>
  );
}
