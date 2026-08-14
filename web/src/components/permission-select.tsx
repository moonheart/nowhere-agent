// Per-session execution-permission selector, rendered at the composer's
// bottom-left. "自动" applies the server's env PERMISSION_* policy (dangerous
// calls gate for an approve/deny card); "完全允许" bypasses that approval gate so
// commands run without prompting. The mode lives in the session's state store
// (per-session, live-synced, reload-safe); on a blank draft it is held locally
// and applied to the session the first message creates.

import type { FC } from "react";
import { ShieldCheck, ShieldAlert, ChevronUp } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { usePermissionModeController } from "@/lib/permission";
import { t } from "@/lib/i18n";
import { cn } from "@/lib/utils";

export const PermissionSelect: FC<{ sessionId: string | null }> = ({ sessionId }) => {
  const [mode, setMode] = usePermissionModeController(sessionId);
  const allowAll = mode === "allow_all";

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        title={t("permission.title")}
        className={cn(
          "flex items-center gap-1 rounded-md px-2 py-1 text-xs outline-none transition-colors",
          allowAll
            ? "text-amber-600 hover:bg-amber-500/10 dark:text-amber-400"
            : "text-muted-foreground hover:bg-muted hover:text-foreground",
        )}
      >
        {allowAll ? <ShieldAlert className="size-3.5" /> : <ShieldCheck className="size-3.5" />}
        <span>{allowAll ? t("permission.allowAll") : t("permission.auto")}</span>
        <ChevronUp className="size-3 opacity-60" />
      </DropdownMenuTrigger>
      {/* side=top: the trigger sits at the composer's bottom edge, so the menu
          opens upward into the thread instead of clipping below the viewport. */}
      <DropdownMenuContent side="top" align="start" sideOffset={6} className="w-44">
        <DropdownMenuRadioGroup
          value={mode}
          onValueChange={(v) => {
            if (v === "auto" || v === "allow_all") setMode(v);
          }}
        >
          <DropdownMenuRadioItem value="auto">
            <span className="flex flex-col">
              <span>{t("permission.auto")}</span>
              <span className="text-[11px] text-muted-foreground">{t("permission.autoHint")}</span>
            </span>
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="allow_all">
            <span className="flex flex-col">
              <span>{t("permission.allowAll")}</span>
              <span className="text-[11px] text-muted-foreground">{t("permission.allowAllHint")}</span>
            </span>
          </DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
};
