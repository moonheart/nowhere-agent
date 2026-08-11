// Platform runtime settings (no-restart configuration): operator knobs that
// used to require editing env and restarting the gateway — the http_request
// allowlist, the query_db business databases, the global webhook target, the
// system-prompt language, and the per-IP rate limits. Writing a value applies
// it immediately (each read path consults the settings snapshot per use);
// clearing returns the key to its env default. Changes are audited.

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
import { api } from "@/lib/api";
import { AsyncSection, ErrorNotice, PageHeader, useAsync } from "@/components/admin/common";

type SettingEntry = {
  key: string;
  value: string | number | null;
  description: string;
};

const listSettings = () =>
  api<{ settings: SettingEntry[] }>("/api/admin/settings");

const putSetting = (key: string, value: string | number | null) =>
  api<void>(`/api/admin/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    body: { value },
  });

export function PlatformSettingsPage() {
  const state = useAsync(listSettings, []);
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  const save = async (key: string, current: string | number | null) => {
    setBusy(key);
    setError(null);
    setSaved(null);
    try {
      // An empty draft clears the override (back to the env default); the
      // effective value shown then reflects the default.
      const raw = (drafts[key] ?? "").trim();
      if (raw === "") {
        await putSetting(key, null);
      } else if (typeof current === "number") {
        await putSetting(key, Number(raw));
      } else {
        await putSetting(key, raw);
      }
      setSaved(key);
      setDrafts((d) => ({ ...d, [key]: "" }));
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  return (
    <>
      <PageHeader
        title="Platform settings"
        description="Runtime configuration — changes apply immediately, no restart. Leave a value empty and save to restore the environment default."
      />
      {error && <ErrorNotice message={error} />}
      <div className="grid gap-4">
        <AsyncSection
          state={state}
          loadingLabel="Loading settings"
        >
          {(data) => (
            <>
              {data.settings.map((s) => (
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
                            {s.value === "" ? "(empty)" : String(s.value)}
                          </span>
                        </Label>
                        <Input
                          id={`setting-${s.key}`}
                          placeholder="New value (empty = restore env default)"
                          value={drafts[s.key] ?? ""}
                          onChange={(e) =>
                            setDrafts((d) => ({ ...d, [s.key]: e.target.value }))
                          }
                        />
                      </div>
                      <Button
                        disabled={busy === s.key}
                        onClick={() => save(s.key, s.value)}
                      >
                        {busy === s.key ? "Saving…" : <><Save /> Save</>}
                      </Button>
                    </div>
                    {saved === s.key && (
                      <p className="text-sm text-muted-foreground">
                        Applied — no restart needed.
                      </p>
                    )}
                  </CardContent>
                </Card>
              ))}
            </>
          )}
        </AsyncSection>
      </div>
    </>
  );
}
