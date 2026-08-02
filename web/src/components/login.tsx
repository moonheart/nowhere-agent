import { useState, type FC, type FormEvent } from "react";
import { AlertCircle } from "lucide-react";
import { login, signup } from "@/lib/auth";
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

export const LoginForm: FC<{ onSuccess: () => void }> = ({ onSuccess }) => {
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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
            {mode === "login" ? "Sign in" : "Create account"}
          </CardTitle>
          <CardDescription>
            {mode === "login"
              ? "Continue to nowhere-agent."
              : "Set up a new nowhere-agent account."}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit}>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="login-email">Email</FieldLabel>
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
                <FieldLabel htmlFor="login-password">Password</FieldLabel>
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
                {busy ? "Working…" : mode === "login" ? "Sign in" : "Sign up"}
              </Button>
              <Button
                type="button"
                variant="link"
                size="sm"
                onClick={() => setMode(mode === "login" ? "signup" : "login")}
              >
                {mode === "login"
                  ? "No account? Sign up"
                  : "Have an account? Sign in"}
              </Button>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>
    </div>
  );
};
