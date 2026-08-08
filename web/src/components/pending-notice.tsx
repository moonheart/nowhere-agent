import type { FC } from "react";
import { X } from "lucide-react";
import { clearNotice, useNotice } from "@/lib/notice";
import { Button } from "@/components/ui/button";

// PendingNotice is the dismissible banner for the pending-interaction 409
// (capability suspend-batch-snapshot): a send rejected because an approval /
// ask_user card is still parked. It points the user at the unresolved card
// rather than failing silently; the rejected message stays in the thread, so
// nothing typed is lost.
export const PendingNotice: FC = () => {
  const notice = useNotice();
  if (notice === null) return null;
  return (
    <div className="flex items-center gap-3 border-b border-amber-500/30 bg-amber-500/10 px-6 py-2.5 text-sm text-amber-700 dark:text-amber-400">
      <span className="min-w-0 flex-1">{notice}</span>
      <Button
        variant="ghost"
        size="icon-sm"
        title="Dismiss"
        onClick={clearNotice}
        className="shrink-0"
      >
        <X />
        <span className="sr-only">Dismiss</span>
      </Button>
    </div>
  );
};
