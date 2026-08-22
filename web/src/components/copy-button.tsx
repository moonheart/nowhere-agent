import { useState, type FC } from "react";
import { Check, Copy } from "lucide-react";
import { Button } from "@/components/ui/button";
import { t } from "@/lib/i18n";
import { cn } from "@/lib/utils";

export const CopyButton: FC<{ text: string; className?: string; size?: "icon-xs" | "icon-sm" | "sm"; withLabel?: boolean }> = ({
  text,
  className,
  size = "icon-xs",
  withLabel = false,
}) => {
  const [copied, setCopied] = useState(false);
  const onCopy = async () => {
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // fallback: create textarea
      try {
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
        setCopied(true);
        setTimeout(() => setCopied(false), 1800);
      } catch {}
    }
  };
  return (
    <Button
      variant="ghost"
      size={withLabel ? "sm" : size}
      onClick={onCopy}
      title={copied ? t("chat.copied") : t("chat.copy")}
      aria-label={copied ? t("chat.copied") : t("chat.copy")}
      className={cn("shrink-0 gap-1.5 text-muted-foreground hover:text-foreground", withLabel && "h-7 px-2 text-xs", className)}
    >
      {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
      {withLabel && <span>{copied ? t("chat.copied") : t("chat.copy")}</span>}
    </Button>
  );
};
