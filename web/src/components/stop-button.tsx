import type { FC } from "react";
import { useComposerRuntime } from "@assistant-ui/react";
import { Square } from "lucide-react";
import { Button } from "@/components/ui/button";
import { getSessionId } from "@/lib/thread";
import { cancelSession } from "@/lib/sessions";
import { t } from "@/lib/i18n";
import { cn } from "@/lib/utils";

// StopButton cancels the session's in-flight run. Unlike ComposerPrimitive.Cancel
// — which only aborts this client's local stream — it ALSO tells the backend to
// cancel the run, so it works symmetrically whether this client submitted the run
// or merely attached to it (multi-tab). The backend Cancel is idempotent, so the
// local abort's onCancel firing cancelSession a second time is harmless.
export const StopButton: FC<{ className?: string }> = ({ className }) => {
  const composer = useComposerRuntime();
  const stop = () => {
    // Tell the backend to cancel the run first (the authoritative stop).
    const id = getSessionId();
    if (id) void cancelSession(id);
    // Then abort the local stream/UI. For a submitted run this also fires the
    // runtime's onCancel (a second, harmless cancelSession); for an attached run
    // it just detaches the local follow.
    composer.cancel();
  };
  return (
    <Button
      type="button"
      variant="secondary"
      size="sm"
      title={t("chat.stopTitle")}
      onClick={stop}
      className={cn(className)}
    >
      <Square className="fill-current" />
      {t("chat.stop")}
    </Button>
  );
};
