// Platform runtime settings (no-restart configuration): operator knobs that
// used to require editing env and restarting the gateway. The page is split
// into tabs (tools, webhooks, LLM, sandbox, permissions, redaction,
// subagents, background tasks); each card shows the key's current effective
// value (env default or the last persisted override) and writes a new value —
// applied immediately on the next use, no restart. Saving with an empty value
// restores the env default. Secret keys (the webhook signing secret) are
// never echoed back: the card shows set/unset and takes a new value to
// overwrite. Changes are audited.

import { useState } from "react";
import { Save } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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

type SettingKind = "string" | "int" | "float" | "bool";

type SettingEntry = {
  key: string;
  group: string;
  kind: SettingKind;
  value: string | number | boolean | null;
  secret: boolean;
  description: string;
};

// Groups in display order; the label doubles as the tab title.
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

const listSettings = () =>
  api<{ settings: SettingEntry[] }>("/api/admin/settings");

const putSetting = (key: string, value: string | number | boolean | null) =>
  api<void>(`/api/admin/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    body: { value },
  });

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
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [bools, setBools] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  const save = async (s: SettingEntry, rawValue: string) => {
    setBusy(s.key);
    setError(null);
    setSaved(null);
    try {
      // An empty draft clears the override (back to the env default); the
      // effective value shown then reflects the default. Secrets use the
      // draft verbatim (empty = clear).
      const raw = rawValue.trim();
      if (s.kind === "bool") {
        await putSetting(s.key, bools[s.key] ?? false);
      } else if (raw === "") {
        await putSetting(s.key, null);
      } else if (s.kind === "int") {
        await putSetting(s.key, Number(raw));
      } else if (s.kind === "float") {
        await putSetting(s.key, Number(raw));
      } else {
        await putSetting(s.key, raw);
      }
      setSaved(s.key);
      setDrafts((d) => ({ ...d, [s.key]: "" }));
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const byGroup = (group: string) =>
    (state.data?.settings ?? []).filter((s) => s.group === group);

  return (
    <>
      <PageHeader
        title="Platform settings"
        description="Runtime configuration — changes apply immediately, no restart. Saving with an empty value restores the environment default."
      />
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading settings">
        {() => (
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
                  {byGroup(g.id).map((s) => (
                    <SettingCard
                      key={s.key}
                      setting={s}
                      draft={drafts[s.key] ?? ""}
                      boolValue={bools[s.key] ?? (s.value === true)}
                      busy={busy === s.key}
                      saved={saved === s.key}
                      onDraft={(v) => setDrafts((d) => ({ ...d, [s.key]: v }))}
                      onBool={(v) => setBools((b) => ({ ...b, [s.key]: v }))}
                      onSave={(raw) => save(s, raw)}
                    />
                  ))}
                </div>
              </TabsContent>
            ))}
          </Tabs>
        )}
      </AsyncSection>
    </>
  );
}

function SettingCard({
  setting: s,
  draft,
  boolValue,
  busy,
  saved,
  onDraft,
  onBool,
  onSave,
}: {
  setting: SettingEntry;
  draft: string;
  boolValue: boolean;
  busy: boolean;
  saved: boolean;
  onDraft: (v: string) => void;
  onBool: (v: boolean) => void;
  onSave: (raw: string) => void;
}) {
  return (
    <Card key={s.key}>
      <CardHeader>
        <CardTitle className="font-mono text-sm">{s.key}</CardTitle>
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
            {s.kind === "bool" ? (
              <div className="flex gap-2">
                {[true, false].map((v) => (
                  <Button
                    key={String(v)}
                    size="sm"
                    variant={boolValue === v ? "default" : "outline"}
                    onClick={() => onBool(v)}
                  >
                    {v ? "true" : "false"}
                  </Button>
                ))}
              </div>
            ) : (
              <Input
                id={`setting-${s.key}`}
                type={s.kind === "int" || s.kind === "float" ? "number" : s.secret ? "password" : "text"}
                step={s.kind === "float" ? "any" : undefined}
                placeholder={
                  s.secret
                    ? "New secret (empty = clear)"
                    : "New value (empty = restore env default)"
                }
                value={draft}
                onChange={(e) => onDraft(e.target.value)}
              />
            )}
          </div>
          <Button
            disabled={busy}
            onClick={() => onSave(draft)}
          >
            {busy ? "Saving…" : <><Save /> Save</>}
          </Button>
        </div>
        {saved && (
          <p className="text-sm text-muted-foreground">
            Applied — no restart needed.
          </p>
        )}
      </CardContent>
    </Card>
  );
}
