// Chat model selector, rendered at the composer's bottom-left next to the
// permission selector. Lists the enabled models of the provider the caller's
// chat runs resolve to (GET /api/chat/models, loaded lazily on first mount);
// the chosen name rides the chat POST body's `model` field ("" = the server's
// resolved default). Hides entirely when no provider serves the caller or the
// list failed to load — a broken picker must never block chat.

import type { FC } from "react";
import { useEffect } from "react";
import { Cpu, ChevronUp } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ensureModels, selectModel, useModelChoices } from "@/lib/models";
import { t } from "@/lib/i18n";

export const ModelSelect: FC = () => {
  const { default: defaultModel, models, selected } = useModelChoices();
  useEffect(() => {
    ensureModels();
  }, []);

  if (models.length === 0) return null;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        title={t("chat.modelTitle")}
        className="flex items-center gap-1 rounded-md px-2 py-1 text-xs outline-none transition-colors text-muted-foreground hover:bg-muted hover:text-foreground"
      >
        <Cpu className="size-3.5" />
        <span className="max-w-28 truncate">{selected || defaultModel || t("chat.model")}</span>
        <ChevronUp className="size-3 opacity-60" />
      </DropdownMenuTrigger>
      {/* side=top: the trigger sits at the composer's bottom edge, so the menu
          opens upward into the thread instead of clipping below the viewport. */}
      <DropdownMenuContent side="top" align="start" sideOffset={6} className="w-56">
        <DropdownMenuRadioGroup
          value={selected || defaultModel}
          onValueChange={(v) => selectModel(v === defaultModel ? defaultModel : v)}
        >
          {models.map((name) => (
            <DropdownMenuRadioItem key={name} value={name}>
              <span className="flex min-w-0 flex-col">
                <span className="truncate">{name}</span>
                {name === defaultModel && (
                  <span className="text-[11px] text-muted-foreground">
                    {t("chat.modelDefault")}
                  </span>
                )}
              </span>
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
};
