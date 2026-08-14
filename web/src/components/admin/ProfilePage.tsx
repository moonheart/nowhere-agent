// Self-service settings: display name, password, teams, and active sessions.

import { useEffect, useState } from "react";
import { Download, LogOut, ShieldCheck, Smartphone } from "lucide-react";
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
  bindPhone,
  changePassword,
  confirmTotp,
  deleteMeAccount,
  disableTotp,
  enableTotp,
  myTokens,
  revokeOtherTokens,
  revokeToken,
  updateMe,
  type SessionToken,
} from "@/lib/admin";
import { clearToken, requestPhoneCode } from "@/lib/auth";
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
      <PhoneCard phone={me.user.phone} onBound={reload} />
      <TotpCard />
      <DataCard />
      <TeamsCard teams={me.teams} />
      <SessionsCard />
      <DangerCard />
    </>
  );
}

// DangerCard is the self-service erasure entry point (PIPL §47): deleting the
// account removes the account and its data irreversibly. The export reminder
// steers the user to the portability copy first.
function DangerCard() {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const doDelete = async () => {
    setBusy(true);
    setError(null);
    try {
      await deleteMeAccount();
      clearToken();
      window.location.assign("/");
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };

  return (
    <Card className="border-destructive/40">
      <CardHeader>
        <CardTitle>Delete account</CardTitle>
        <CardDescription>
          Removes your account and all of its data — conversations, memories,
          uploads — irreversibly. Export your data first if you may need it.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <ErrorNotice message={error} />}
        {confirming ? (
          <div className="flex items-center gap-3">
            <Button variant="destructive" disabled={busy} onClick={doDelete}>
              {busy ? "Deleting…" : "Yes, delete my account and data"}
            </Button>
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => setConfirming(false)}
            >
              Cancel
            </Button>
          </div>
        ) : (
          <Button variant="outline" onClick={() => setConfirming(true)}>
            Delete my account…
          </Button>
        )}
      </CardContent>
    </Card>
  );
}

// TotpCard is the second-factor (MFA) self-service: enroll (secret shown
// once, typically scanned into an authenticator app), confirm, disable.
function TotpCard() {
  const [phase, setPhase] = useState<"idle" | "enrolled" | "enabled">("idle");
  const [secret, setSecret] = useState("");
  const [uri, setUri] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const start = async () => {
    setBusy(true);
    setError(null);
    try {
      const res = await enableTotp();
      setSecret(res.secret);
      setUri(res.uri);
      setCode(""); // a previous cycle's code must not leak into the new one
      setDone(false);
      setPhase("enrolled");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const confirm = async () => {
    setBusy(true);
    setError(null);
    try {
      await confirmTotp(code);
      // Drop the used code so the disable input starts empty — a 30s TOTP
      // window means the enable code is stale by the time Disable is clicked.
      setCode("");
      setPhase("enabled");
      setDone(true);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const disable = async () => {
    setBusy(true);
    setError(null);
    try {
      await disableTotp(code);
      setPhase("idle");
      setCode("");
      setSecret("");
      setUri("");
      setDone(true);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Smartphone className="size-4 text-primary" />
          Two-step verification
        </CardTitle>
        <CardDescription>
          An authenticator app code is required in addition to your password —
          recommended for administrators.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {error && <ErrorNotice message={error} />}
        {done && <p className="text-sm text-muted-foreground">Saved.</p>}

        {phase === "idle" && (
          <Button variant="outline" disabled={busy} onClick={start}>
            Set up two-step verification…
          </Button>
        )}

        {phase === "enrolled" && (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">
              Scan this into your authenticator app (or enter the secret
              manually), then confirm with a code:
            </p>
            <div className="rounded-lg border border-border bg-muted/40 px-3 py-2 font-mono text-xs break-all">
              {uri || secret}
            </div>
            <div className="flex items-end gap-3">
              <div className="flex-1 space-y-1.5">
                <Label htmlFor="totp-confirm">Code</Label>
                <Input
                  id="totp-confirm"
                  inputMode="numeric"
                  maxLength={6}
                  placeholder="123456"
                  value={code}
                  onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                />
              </div>
              <Button disabled={busy || code.length !== 6} onClick={confirm}>
                Confirm
              </Button>
            </div>
          </div>
        )}

        {phase === "enabled" && (
          <div className="space-y-3">
            <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2 text-sm">
              <ShieldCheck className="size-4 text-primary" />
              Two-step verification is on.
            </div>
            <div className="flex items-end gap-3">
              <div className="flex-1 space-y-1.5">
                <Label htmlFor="totp-disable">Current code</Label>
                <Input
                  id="totp-disable"
                  inputMode="numeric"
                  maxLength={6}
                  placeholder="123456"
                  value={code}
                  onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                />
              </div>
              <Button
                variant="outline"
                disabled={busy || code.length !== 6}
                onClick={disable}
              >
                Disable
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
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

// PhoneCard is the phone-binding self-service: a bound mobile number lets the
// account recover its password from the login page (phone + SMS code). The
// number shown is masked by the server; only the last four digits are visible.
function PhoneCard({
  phone,
  onBound,
}: {
  phone?: string;
  onBound: () => void;
}) {
  const [number, setNumber] = useState("");
  const [code, setCode] = useState("");
  const [codeSent, setCodeSent] = useState(false);
  const [cooldown, setCooldown] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  // Resend cooldown countdown (60s), mirroring the server's OTP cooldown.
  useEffect(() => {
    if (cooldown <= 0) return;
    const id = setInterval(() => setCooldown((c) => Math.max(0, c - 1)), 1000);
    return () => clearInterval(id);
  }, [cooldown]);

  const send = async () => {
    setBusy(true);
    setError(null);
    try {
      await requestPhoneCode(number);
      setCodeSent(true);
      setCooldown(60);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const bind = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await bindPhone(number, code);
      setDone(true);
      setCode("");
      onBound();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Smartphone className="size-4 text-primary" />
          Phone number
        </CardTitle>
        <CardDescription>
          A bound phone number lets you reset a forgotten password from the
          sign-in screen. The number is verified with a one-time SMS code.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {phone ? (
          <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2 text-sm">
            <ShieldCheck className="size-4 text-primary" />
            Bound: {phone}
            <button
              type="button"
              className="ml-auto text-xs text-muted-foreground underline"
              onClick={() => setNumber("")}
            >
              Change
            </button>
          </div>
        ) : null}
        {error && <ErrorNotice message={error} />}
        {done && <p className="text-sm text-muted-foreground">Saved.</p>}
        <form onSubmit={bind} className="space-y-3">
          <div className="flex items-end gap-3">
            <div className="flex-1 space-y-1.5">
              <Label htmlFor="phone-number">Mobile number</Label>
              <Input
                id="phone-number"
                type="tel"
                inputMode="numeric"
                placeholder="13800138000"
                value={number}
                onChange={(e) => setNumber(e.target.value)}
              />
            </div>
            {codeSent ? null : (
              <Button
                type="button"
                variant="outline"
                disabled={busy || cooldown > 0 || number.trim() === ""}
                onClick={send}
              >
                {cooldown > 0 ? `Resend in ${cooldown}s` : "Send code"}
              </Button>
            )}
          </div>
          {codeSent && (
            <>
              <div className="flex items-end gap-3">
                <div className="flex-1 space-y-1.5">
                  <Label htmlFor="phone-code">Verification code</Label>
                  <Input
                    id="phone-code"
                    inputMode="numeric"
                    maxLength={6}
                    placeholder="123456"
                    value={code}
                    onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                  />
                </div>
                <Button type="submit" disabled={busy || code.length !== 6}>
                  {busy ? "Binding…" : "Bind"}
                </Button>
              </div>
              <Button
                type="button"
                variant="link"
                size="sm"
                className="px-0"
                onClick={() => {
                  setCodeSent(false);
                  setCooldown(0);
                }}
              >
                Use a different number
              </Button>
            </>
          )}
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
