import { useState, type FC, type FormEvent } from "react";
import { login, signup } from "@/lib/auth";

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
    <div className="flex h-full items-center justify-center">
      <form
        onSubmit={submit}
        className="w-full max-w-sm space-y-4 rounded-2xl border border-neutral-200 p-6"
      >
        <h1 className="text-lg font-semibold">
          {mode === "login" ? "Sign in" : "Create account"}
        </h1>

        <input
          type="email"
          required
          autoComplete="email"
          placeholder="Email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          className="w-full rounded-xl border border-neutral-300 px-3 py-2 outline-none focus:border-violet-500"
        />
        <input
          type="password"
          required
          autoComplete={mode === "login" ? "current-password" : "new-password"}
          placeholder="Password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          className="w-full rounded-xl border border-neutral-300 px-3 py-2 outline-none focus:border-violet-500"
        />

        {error && <p className="text-sm text-red-600">{error}</p>}

        <button
          type="submit"
          disabled={busy}
          className="w-full rounded-xl bg-violet-600 px-4 py-2 text-white disabled:opacity-40"
        >
          {busy ? "…" : mode === "login" ? "Sign in" : "Sign up"}
        </button>

        <button
          type="button"
          onClick={() => setMode(mode === "login" ? "signup" : "login")}
          className="w-full text-center text-sm text-violet-600 hover:underline"
        >
          {mode === "login"
            ? "No account? Sign up"
            : "Have an account? Sign in"}
        </button>
      </form>
    </div>
  );
};
