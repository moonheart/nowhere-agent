import { useCallback, useEffect, useMemo, useState } from "react";
import { MessageSquare, Plus, Search, Trash2 } from "lucide-react";
import { deleteSession, listSessions, relTime, type SessionSummary } from "@/lib/sessions";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "@/components/ui/input-group";
import {
  Item,
  ItemContent,
  ItemDescription,
  ItemMedia,
  ItemTitle,
} from "@/components/ui/item";
import { ScrollArea } from "@/components/ui/scroll-area";
import { cn } from "@/lib/utils";

type Props = {
  currentId: string | null;
  onSelect: (id: string) => void;
  onNew: () => void;
  // Called after the current session was deleted (so the app resets to a fresh
  // thread instead of pointing at a gone session).
  onDeleteCurrent: () => void;
  // refreshToken bumps to re-fetch (e.g. after a new session is created).
  refreshToken: number;
};

// SessionList is the left sidebar of conversations. Selecting one switches the
// active thread; "New chat" starts a fresh session; the trash icon deletes one.
export const SessionList = ({ currentId, onSelect, onNew, onDeleteCurrent, refreshToken }: Props) => {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [query, setQuery] = useState("");

  const refresh = useCallback(async () => {
    setSessions(await listSessions());
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh, refreshToken]);

  const handleDelete = async (id: string) => {
    if (!(await deleteSession(id))) return;
    if (id === currentId) {
      onDeleteCurrent();
    } else {
      void refresh();
    }
  };

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return sessions;
    return sessions.filter((s) => (s.title || "Untitled").toLowerCase().includes(q));
  }, [sessions, query]);

  return (
    <aside className="flex h-full w-64 flex-col border-r border-border bg-muted/50">
      <div className="space-y-2 border-b border-border p-3">
        <Button size="lg" className="w-full" onClick={onNew}>
          <Plus />
          New chat
        </Button>
        {sessions.length > 0 && (
          <InputGroup className="bg-background">
            <InputGroupAddon>
              <Search />
            </InputGroupAddon>
            <InputGroupInput
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search chats"
              aria-label="Search chats"
            />
          </InputGroup>
        )}
      </div>

      <ScrollArea className="min-h-0 flex-1">
        <div className="p-2">
          {sessions.length === 0 && (
            <Empty className="p-4">
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <MessageSquare />
                </EmptyMedia>
                <EmptyTitle>No conversations yet</EmptyTitle>
                <EmptyDescription>
                  Start one with “New chat”.
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
          {sessions.length > 0 && filtered.length === 0 && (
            <Empty className="p-4">
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <Search />
                </EmptyMedia>
                <EmptyTitle>No matches</EmptyTitle>
                <EmptyDescription>Nothing matches “{query}”.</EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
          <ul className="flex flex-col gap-0.5">
            {filtered.map((s) => {
              const active = s.id === currentId;
              return (
                <li key={s.id} className="group relative">
                  <Item
                    size="sm"
                    className={cn(
                      "cursor-pointer pr-9 text-left",
                      active
                        ? "bg-primary/10 text-foreground"
                        : "text-foreground/80 hover:bg-muted",
                    )}
                    render={
                      <button type="button" onClick={() => onSelect(s.id)} />
                    }
                  >
                    <ItemMedia variant="icon">
                      <MessageSquare
                        className={
                          active ? "text-primary" : "text-muted-foreground"
                        }
                      />
                    </ItemMedia>
                    {/* min-w-0 on the content column: without it the flex item
                        refuses to shrink below its text width and the title
                        spills out of the w-64 sidebar instead of truncating. */}
                    <ItemContent className="min-w-0 gap-0.5">
                      <ItemTitle className="w-full min-w-0">
                        <span className="truncate">
                          {s.title || "Untitled"}
                        </span>
                      </ItemTitle>
                      <ItemDescription className="text-xs">
                        {relTime(s.updatedAt)}
                      </ItemDescription>
                    </ItemContent>
                  </Item>
                  <Button
                    variant="ghost"
                    size="icon-xs"
                    aria-label="Delete conversation"
                    title="Delete conversation"
                    onClick={(e) => {
                      e.stopPropagation();
                      void handleDelete(s.id);
                    }}
                    className="absolute top-1/2 right-1.5 -translate-y-1/2 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100 hover:bg-destructive/10 hover:text-destructive focus-visible:opacity-100"
                  >
                    <Trash2 />
                  </Button>
                </li>
              );
            })}
          </ul>
        </div>
      </ScrollArea>
    </aside>
  );
};
