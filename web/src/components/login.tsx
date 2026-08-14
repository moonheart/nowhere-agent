import { useEffect, useState, type FC, type FormEvent } from "react";
import { AlertCircle, KeyRound, ShieldCheck, Smartphone } from "lucide-react";
import {
  completeTotpLogin,
  login,
  phoneAuthAvailable,
  requestEmailResetCode,
  requestPhoneCode,
  resetPasswordWithEmail,
  resetPasswordWithPhone,
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
  // Self-service password recovery: the forgot link switches to a reset flow.
  // The recovery channel is the bound phone when phone login is enabled, and
  // the account email otherwise (codes are printed to the server log until a
  // mail channel exists — self-host/dev path).
  const [resetMode, setResetMode] = useState(false);
  const [newPassword, setNewPassword] = useState("");
  const [resetDone, setResetDone] = useState(false);
  // Second-factor challenge state (MFA): the password half succeeded (or the
  // IdP authenticated the account), the account demands an authenticator code
  // before any token is issued.
  const [totpChallenge, setTotpChallenge] = useState<string | null>(
    initialTotpToken,
  );
  const [totpCode, setTotpCode] = useState("");

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

  const submitReset = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      if (phoneEnabled) {
        await resetPasswordWithPhone(phone, code, newPassword);
      } else {
        await resetPasswordWithEmail(email, code, newPassword);
      }
      setResetDone(true);
      setCodeSent(false);
      setCode("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  // sendResetCode mints the recovery code for the active channel (bound phone
  // when phone login is enabled, account email otherwise).
  const sendResetCode = async () => {
    setError(null);
    setBusy(true);
    try {
      if (phoneEnabled) {
        await requestPhoneCode(phone);
      } else {
        await requestEmailResetCode(email);
      }
      setCodeSent(true);
      setCooldown(60);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const backToEmail = () => {
    setPhoneMode(false);
    setResetMode(false);
    setCodeSent(false);
    setResetDone(false);
    setCode("");
    setNewPassword("");
  };

  return (
    <div className="flex h-full items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>
            {resetMode
              ? t("login.resetTitle")
              : phoneMode
                ? t("login.phone")
                : mode === "login"
                  ? t("login.title")
                  : t("login.titleSignup")}
          </CardTitle>
          <CardDescription>
            {resetMode
              ? phoneEnabled
                ? t("login.resetSubtitle")
                : t("login.resetSubtitleEmail")
              : phoneMode
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
            <form onSubmit={submitTotp} className="space-y-4">              <FieldGroup>
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
                    placeholder={t("login.otpPlaceholder")}
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
          ) : resetMode ? (
            <form onSubmit={submitReset} className="space-y-4">
              <FieldGroup>
                {phoneEnabled ? (
                  <Field>
                    <FieldLabel htmlFor="reset-phone">{t("login.phone")}</FieldLabel>
                    <Input
                      id="reset-phone"
                      type="tel"
                      required
                      autoComplete="tel"
                      inputMode="numeric"
                      placeholder="13800138000"
                      value={phone}
                      onChange={(e) => setPhone(e.target.value)}
                    />
                  </Field>
                ) : (
                  <Field>
                    <FieldLabel htmlFor="reset-email">{t("login.email")}</FieldLabel>
                    <Input
                      id="reset-email"
                      type="email"
                      required
                      autoComplete="email"
                      placeholder="you@example.com"
                      value={email}
                      onChange={(e) => setEmail(e.target.value)}
                    />
                  </Field>
                )}
                {codeSent && (
                  <>
                    <Field>
                      <FieldLabel htmlFor="reset-otp">{t("login.code")}</FieldLabel>
                      <Input
                        id="reset-otp"
                        type="text"
                        required
                        inputMode="numeric"
                        maxLength={6}
                        placeholder={t("login.otpPlaceholder")}
                        value={code}
                        onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                      />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="reset-password">
                        {t("login.newPassword")}
                      </FieldLabel>
                      <Input
                        id="reset-password"
                        type="password"
                        required
                        autoComplete="new-password"
                        placeholder="••••••••"
                        value={newPassword}
                        onChange={(e) => setNewPassword(e.target.value)}
                      />
                    </Field>
                  </>
                )}
                {error && (
                  <Alert variant="destructive">
                    <AlertCircle />
                    <AlertDescription>{error}</AlertDescription>
                  </Alert>
                )}
                {resetDone ? (
                  <p className="text-sm text-muted-foreground">
                    {t("login.resetSuccess")}
                  </p>
                ) : codeSent ? (
                  <Button type="submit" size="lg" disabled={busy || code.length !== 6 || newPassword.length < 8}>
                    {busy ? t("login.busy") : t("login.resetSubmit")}
                  </Button>
                ) : (
                  <Button
                    type="button"
                    size="lg"
                    disabled={
                      busy ||
                      cooldown > 0 ||
                      (phoneEnabled ? phone.trim() === "" : email.trim() === "")
                    }
                    onClick={sendResetCode}
                  >
                    {cooldown > 0
                      ? `${t("login.resendIn")} ${cooldown}s`
                      : t("login.sendCode")}
                  </Button>
                )}
                <Button type="button" variant="link" size="sm" onClick={backToEmail}>
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
                      placeholder={t("login.otpPlaceholder")}
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
                      onClick={() => {
                        setResetMode(true);
                        setCodeSent(false);
                      }}
                    >
                      {t("login.forgotPassword")}
                    </Button>
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
