// SecretField edits a secret setting: it shows only set/unset status, takes
// a new value in a password field, and offers a random-value generator
// (webhook signing secret). Saving empty clears the override.

import { useState } from "react";
import { RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

function randomBytes(n: number): string {
  const b = new Uint8Array(n);
  crypto.getRandomValues(b);
  let out = "";
  for (let i = 0; i < b.length; i++) out += String.fromCharCode(b[i]);
  return btoa(out);
}

export function SecretField({
  isSet,
  value,
  onChange,
  placeholder,
}: {
  isSet: boolean;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
}) {
  const [show, setShow] = useState(false);
  return (
    <div className="flex w-full items-center gap-2">
      <Input
        type={show ? "text" : "password"}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={
          placeholder ??
          (isSet ? "New secret (empty = clear)" : "Secret value")
        }
        className="font-mono text-xs"
      />
      <Button
        variant="outline"
        size="sm"
        onClick={() => onChange(randomBytes(32))}
        title="Generate a random 32-byte secret (base64)"
      >
        <RefreshCw /> Generate
      </Button>
      <Button variant="ghost" size="sm" onClick={() => setShow(!show)}>
        {show ? "Hide" : "Show"}
      </Button>
    </div>
  );
}
