import { useEffect, useState, type FC, type FormEvent } from "react";
import { AlertCircle, KeyRound, ShieldCheck, Smartphone } from "lucide-react";
import {
  completeTotpLogin,
  login,
  phoneAuthAvailable,
  requestPhoneCode,
  signup,
  verifyPhoneCode,
} from "@/lib/auth";
import { t } from "@/lib/i18n";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";

// ssoAvailable probes the server for OIDC single-sign-on (P1-2). The route
// exists only when SSO is configured, so a 404/network error reads as "off" and
// the SSO button simply does not render — password sign-in is always there.
async function ssoAvailable(): Promise<boolean> {
  try {
    const res = await fetch("/auth/oidc/enabled");
    return res.ok;
  } catch {
    return false;
  }
}

export const LoginForm: FC<{
  onSuccess: () => void;
  ssoError?: string | null;
  initialTotpToken?: string | null;
}> = ({ onSuccess, ssoError = null, initialTotpToken = null }) => {
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [sso, setSso] = useState(false);
  const [phone, setPhone] = useState("");
  const [code, setCode] = useState("");
  const [phoneMode, setPhoneMode] = useState(false);
  const [phoneEnabled, setPhoneEnabled] = useState(false);
  const [codeSent, setCodeSent] = useState(false);
  const [cooldown, setCooldown] = useState(0);
  // Second-factor challenge state (MFA): the password half succeeded (or the
  // IdP authenticated the account), the account demands an authenticator code
  // before any token is issued.
  const [totpChallenge, setTotpChallenge] = useState<string | null>(
    initialTotpToken,
  );
  const [totpCode, setTotpCode] = useState("");
  const [showForgotHint, setShowForgotHint] = useState(false);

  useEffect(() => {
    let live = true;
    ssoAvailable().then((ok) => {
      if (live) setSso(ok);
    });
    phoneAuthAvailable().then((ok) => {
      if (live) setPhoneEnabled(ok);
    });
    return () => {
      live = false;
    };
  }, []);

  // Resend cooldown countdown (60s), mirroring the server's OTP cooldown.
  useEffect(() => {
    if (cooldown <= 0) return;
    const id = setInterval(() => setCooldown((c) => Math.max(0, c - 1)), 1000);
    return () => clearInterval(id);
  }, [cooldown]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      if (mode === "login") {
        const result = await login(email, password);
        if (result.totp_required) {
          setTotpChallenge(result.totp_token);
          return;
        }
      } else {
        await signup(email, password);
      }
      onSuccess();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const submitTotp = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      if (totpChallenge) await completeTotpLogin(totpChallenge, totpCode);
      onSuccess();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const sendCode = async () => {
    setError(null);
    setBusy(true);
    try {
      await requestPhoneCode(phone);
      setCodeSent(true);
      setCooldown(60);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const verify = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await verifyPhoneCode(phone, code);
      onSuccess();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex h-full items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>
            {phoneMode
              ? t("login.phone")
              : mode === "login"
                ? t("login.title")
                : t("login.titleSignup")}
          </CardTitle>
          <CardDescription>
            {phoneMode
              ? t("login.phoneSubtitle")
              : mode === "login"
                ? t("login.subtitle")
                : t("login.subtitleSignup")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {ssoError && (
            <Alert variant="destructive" className="mb-4">
              <AlertCircle />
              <AlertDescription>{ssoError}</AlertDescription>
            </Alert>
          )}
          {phoneEnabled && (
            <div className="mb-4 flex gap-2">
              <Button
                type="button"
                variant={phoneMode ? "default" : "outline"}
                size="sm"
                className="flex-1"
                onClick={() => setPhoneMode(false)}
              >
                <KeyRound />
                {t("login.email")}
              </Button>
              <Button
                type="button"
                variant={phoneMode ? "outline" : "default"}
                size="sm"
                className="flex-1"
                onClick={() => setPhoneMode(true)}
              >
                <Smartphone />
                {t("login.phone")}
              </Button>
            </div>
          )}
          {sso && !phoneMode && (
            <div className="mb-4">
              <Button
                type="button"
                variant="outline"
                size="lg"
                className="w-full"
                onClick={() => {
                  window.location.href = "/auth/oidc/login";
                }}
              >
                <KeyRound />
                {t("login.sso")}
              </Button>
              <div className="my-4 flex items-center gap-3 text-xs text-muted-foreground">
                <span className="h-px flex-1 bg-border" />
                {t("login.orEmail")}
                <span className="h-px flex-1 bg-border" />
              </div>
            </div>
          )}

          {totpChallenge ? (
            <form onSubmit={submitTotp} className="space-y-4">
              <FieldGroup>
                <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2 text-sm">
                  <ShieldCheck className="size-4 text-primary" />
                  {t("login.totpHint")}
                </div>
                <Field>
                  <FieldLabel htmlFor="login-totp">{t("login.totpCode")}</FieldLabel>
                  <Input
                    id="login-totp"
                    type="text"
                    required
                    inputMode="numeric"
                    autoFocus
                    maxLength={6}
                    placeholder="6 位验证码"
                    value={totpCode}
                    onChange={(e) => setTotpCode(e.target.value.replace(/\D/g, ""))}
                  />
                </Field>
                {error && (
                  <Alert variant="destructive">
                    <AlertCircle />
                    <AlertDescription>{error}</AlertDescription>
                  </Alert>
                )}
                <Button type="submit" size="lg" disabled={busy || totpCode.length !== 6}>
                  {busy ? t("login.busy") : t("login.submit")}
                </Button>
                <Button
                  type="button"
                  variant="link"
                  size="sm"
                  onClick={() => setTotpChallenge(null)}
                >
                  {t("login.backToEmail")}
                </Button>
              </FieldGroup>
            </form>
          ) : phoneMode ? (
            <form onSubmit={verify} className="space-y-4">
              <FieldGroup>
                <Field>
                  <FieldLabel htmlFor="login-phone">{t("login.phone")}</FieldLabel>
                  <Input
                    id="login-phone"
                    type="tel"
                    required
                    autoComplete="tel"
                    inputMode="numeric"
                    placeholder="13800138000"
                    value={phone}
                    onChange={(e) => setPhone(e.target.value)}
                  />
                </Field>
                {codeSent && (
                  <Field>
                    <FieldLabel htmlFor="login-otp">{t("login.code")}</FieldLabel>
                    <Input
                      id="login-otp"
                      type="text"
                      required
                      inputMode="numeric"
                      maxLength={6}
                      placeholder="6 位验证码"
                      value={code}
                      onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                    />
                  </Field>
                )}
                {error && (
                  <Alert variant="destructive">
                    <AlertCircle />
                    <AlertDescription>{error}</AlertDescription>
                  </Alert>
                )}
                {codeSent ? (
                  <Button type="submit" size="lg" disabled={busy || code.length !== 6}>
                    {busy ? t("login.busy") : t("login.submit")}
                  </Button>
                ) : (
                  <Button
                    type="button"
                    size="lg"
                    disabled={busy || cooldown > 0 || phone.trim() === ""}
                    onClick={sendCode}
                  >
                    {cooldown > 0
                      ? `${t("login.resendIn")} ${cooldown}s`
                      : t("login.sendCode")}
                  </Button>
                )}
                <Button
                  type="button"
                  variant="link"
                  size="sm"
                  onClick={() => {
                    setPhoneMode(false);
                    setCodeSent(false);
                  }}
                >
                  {t("login.backToEmail")}
                </Button>
              </FieldGroup>
            </form>
          ) : (
            <form onSubmit={submit}>
              <FieldGroup>
                <Field>
                  <FieldLabel htmlFor="login-email">{t("login.email")}</FieldLabel>
                  <Input
                    id="login-email"
                    type="email"
                    required
                    autoComplete="email"
                    placeholder="you@example.com"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                  />
                </Field>
                <Field>
                  <FieldLabel htmlFor="login-password">{t("login.password")}</FieldLabel>
                  <Input
                    id="login-password"
                    type="password"
                    required
                    autoComplete={
                      mode === "login" ? "current-password" : "new-password"
                    }
                    placeholder="••••••••"
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                  />
                </Field>
                {mode === "login" && (
                  <div>
                    <Button
                      type="button"
                      variant="link"
                      size="sm"
                      className="px-0"
                      onClick={() => setShowForgotHint((s) => !s)}
                    >
                      {t("login.forgotPassword")}
                    </Button>
                    {showForgotHint && (
                      <p className="text-xs text-muted-foreground">
                        {t("login.forgotHint")}
                      </p>
                    )}
                  </div>
                )}

                {error && (
                  <Alert variant="destructive">
                    <AlertCircle />
                    <AlertDescription>{error}</AlertDescription>
                  </Alert>
                )}

                <Button type="submit" size="lg" disabled={busy}>
                  {busy
                    ? t("login.busy")
                    : mode === "login"
                      ? t("login.submit")
                      : t("login.submitSignup")}
                </Button>
                <Button
                  type="button"
                  variant="link"
                  size="sm"
                  onClick={() => setMode(mode === "login" ? "signup" : "login")}
                >
                  {mode === "login"
                    ? t("login.toggleToSignup")
                    : t("login.toggleToLogin")}
                </Button>
              </FieldGroup>
            </form>
          )}
        </CardContent>
      </Card>
    </div>
  );
};
