import { useEffect, useState, type FC, type FormEvent } from "react";
import { AlertCircle, KeyRound } from "lucide-react";
import { login, signup } from "@/lib/auth";
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

export const LoginForm: FC<{ onSuccess: () => void; ssoError?: string | null }> = ({
  onSuccess,
  ssoError = null,
}) => {
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [sso, setSso] = useState(false);

  useEffect(() => {
    let live = true;
    ssoAvailable().then((ok) => {
      if (live) setSso(ok);
    });
    return () => {
      live = false;
    };
  }, []);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      if (mode === "login") await login(email, password);
      else await signup(email, password);
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
            {mode === "login" ? t("login.title") : t("login.titleSignup")}
          </CardTitle>
          <CardDescription>
            {mode === "login" ? t("login.subtitle") : t("login.subtitleSignup")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {ssoError && (
            <Alert variant="destructive" className="mb-4">
              <AlertCircle />
              <AlertDescription>{ssoError}</AlertDescription>
            </Alert>
          )}
          {sso && (
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
        </CardContent>
      </Card>
    </div>
  );
};
