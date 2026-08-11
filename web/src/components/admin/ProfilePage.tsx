// Self-service settings: display name, password, teams, and active sessions.

import { useState } from "react";
import { Download, LogOut, ShieldCheck } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api } from "@/lib/api";
import { t } from "@/lib/i18n";
import {
  changePassword,
  myTokens,
  revokeOtherTokens,
  revokeToken,
  updateMe,
  type SessionToken,
} from "@/lib/admin";
import { clearToken } from "@/lib/auth";
import { useConsoleMe } from "@/components/admin/AdminLayout";
import {
  AsyncSection,
  ErrorNotice,
  formatDateTime,
  PageHeader,
  RoleBadge,
  useAsync,
} from "@/components/admin/common";

export function ProfilePage() {
  const { me, reload } = useConsoleMe();

  return (
    <>
      <PageHeader
        title="Profile"
        description="Your account, the teams you belong to, and the devices signed in as you."
      />
      <IdentityCard
        email={me.user.email}
        displayName={me.user.display_name}
        isAdmin={me.user.platform_role === "admin"}
        onSaved={reload}
      />
      <PasswordCard />
      <DataCard />
      <TeamsCard teams={me.teams} />
      <SessionsCard />
    </>
  );
}

// DataCard is the self-service data-portability entry point (PIPL §45 export
// right): the account owner downloads their full data footprint as JSON.
function DataCard() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const exportData = async () => {
    setBusy(true);
    setError(null);
    try {
      const blob = await api<Blob>("/api/me/export", { raw: true });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `nowhere-agent-export-${new Date().toISOString().slice(0, 10)}.json`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>My data</CardTitle>
        <CardDescription>
          Download your conversations, memories, uploads, and usage — the
          portability copy of your data on this platform.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {error && <ErrorNotice message={error} />}
        <Button variant="outline" disabled={busy} onClick={exportData}>
          <Download />
          {busy ? "Preparing…" : t("profile.exportData")}
        </Button>
      </CardContent>
    </Card>
  );
}

function IdentityCard({
  email,
  displayName,
  isAdmin,
  onSaved,
}: {
  email: string;
  displayName: string;
  isAdmin: boolean;
  onSaved: () => void;
}) {
  const [name, setName] = useState(displayName);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSaving(true);
    setError(null);
    setSaved(false);
    try {
      await updateMe(name.trim());
      setSaved(true);
      onSaved();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Account</CardTitle>
        <CardDescription>
          Your email is the address teams use to add you; it cannot be changed here.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="email">Email</Label>
              <Input id="email" value={email} disabled readOnly />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="display-name">Display name</Label>
              <Input
                id="display-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="How your name appears to teammates"
              />
            </div>
          </div>
          {isAdmin && (
            <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2 text-sm">
              <ShieldCheck className="size-4 text-primary" />
              You hold the platform administrator role.
            </div>
          )}
          {error && <ErrorNotice message={error} />}
          <div className="flex items-center gap-3">
            <Button type="submit" disabled={saving || name.trim() === "" || name === displayName}>
              {saving ? "Saving…" : "Save"}
            </Button>
            {saved && <span className="text-sm text-muted-foreground">Saved.</span>}
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

function PasswordCard() {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (next !== confirm) {
      setError("the new passwords do not match");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await changePassword(current, next);
      // The server revoked every token, including this request's. Clearing the
      // stored one and bouncing to the login screen is honest about that,
      // rather than letting the next call fail with a surprise 401.
      setDone(true);
      clearToken();
      setTimeout(() => window.location.assign("/"), 1500);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Password</CardTitle>
        <CardDescription>
          Changing your password signs out every device, including this one.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-3">
            <div className="space-y-1.5">
              <Label htmlFor="current-pw">Current password</Label>
              <Input
                id="current-pw"
                type="password"
                autoComplete="current-password"
                value={current}
                onChange={(e) => setCurrent(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="new-pw">New password</Label>
              <Input
                id="new-pw"
                type="password"
                autoComplete="new-password"
                value={next}
                onChange={(e) => setNext(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="confirm-pw">Confirm</Label>
              <Input
                id="confirm-pw"
                type="password"
                autoComplete="new-password"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
              />
            </div>
          </div>
          <p className="text-xs text-muted-foreground">At least 8 characters.</p>
          {error && <ErrorNotice message={error} />}
          {done && (
            <p className="text-sm text-muted-foreground">
              Password changed — signing you out…
            </p>
          )}
          <Button type="submit" disabled={busy || !current || next.length < 8}>
            {busy ? "Changing…" : "Change password"}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

function TeamsCard({
  teams,
}: {
  teams: { id: string; name: string; role: "owner" | "admin" | "member" }[];
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Teams</CardTitle>
        <CardDescription>
          Teams share skills, memories, and provider keys with their members.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {teams.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            You do not belong to any team yet.
          </p>
        ) : (
          <ul className="divide-y divide-border">
            {teams.map((t) => (
              <li key={t.id} className="flex items-center justify-between py-2">
                <span className="text-sm">{t.name}</span>
                <RoleBadge role={t.role} />
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

function SessionsCard() {
  const state = useAsync(() => myTokens(), []);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const revokeOne = async (t: SessionToken) => {
    setBusy(true);
    setError(null);
    try {
      await revokeToken(t.id);
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const revokeRest = async () => {
    setBusy(true);
    setError(null);
    try {
      await revokeOtherTokens();
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Active sessions</CardTitle>
        <CardDescription>
          Each sign-in issues a token valid for 30 days. Revoking one signs that
          device out immediately.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {error && <ErrorNotice message={error} />}
        <AsyncSection state={state} loadingLabel="Loading sessions">
          {(data) => (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Signed in</TableHead>
                    <TableHead>Expires</TableHead>
                    <TableHead className="w-24" />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.tokens.map((t) => (
                    <TableRow key={t.id}>
                      <TableCell>
                        <span className="text-sm">{formatDateTime(t.created_at)}</span>
                        {t.current && (
                          <Badge variant="secondary" className="ml-2">
                            This device
                          </Badge>
                        )}
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {formatDateTime(t.expires_at)}
                      </TableCell>
                      <TableCell className="text-right">
                        {!t.current && (
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={busy}
                            onClick={() => revokeOne(t)}
                          >
                            Revoke
                          </Button>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              {data.tokens.length > 1 && (
                <Button variant="outline" size="sm" disabled={busy} onClick={revokeRest}>
                  <LogOut />
                  Sign out everywhere else
                </Button>
              )}
            </>
          )}
        </AsyncSection>
      </CardContent>
    </Card>
  );
}
