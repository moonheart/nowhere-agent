import { useCallback, useEffect, useRef, useState } from "react";
import { Loader2, MessageSquare, Plus, Search, Trash2 } from "lucide-react";
import {
  cancelSession,
  deleteSession,
  listSessions,
  relTime,
  type SessionSummary,
} from "@/lib/sessions";
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
import { t } from "@/lib/i18n";
import { reportNotice } from "@/lib/notice";
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
// The list is loaded from the backend in pages of SESSION_PAGE_SIZE, newest
// first; scrolling near the bottom fetches the next page automatically.
export const SessionList = ({ currentId, onSelect, onNew, onDeleteCurrent, refreshToken }: Props) => {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [nextCursor, setNextCursor] = useState("");
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [query, setQuery] = useState("");
  // loadError is true when the last refresh FAILED (listSessions returned
  // null) — distinct from an empty list, which is a legitimate "no
  // conversations yet". Shows an error/retry banner instead of the empty state.
  const [loadError, setLoadError] = useState(false);
  // debounced is the search term actually sent to the backend (250ms after the
  // last keystroke). queryRef tracks the latest issued term so a response that
  // raced a newer search is dropped instead of overwriting its results.
  const [debounced, setDebounced] = useState("");
  const queryRef = useRef("");
  // seqRef counts refreshes: every refresh (new search, session created or
  // deleted) bumps it, so a loadMore page that was in flight during the
  // refresh — its cursor belongs to the OLD list — is dropped instead of
  // appending deleted sessions or duplicates onto the new first page.
  const seqRef = useRef(0);
  // Sentinel at the bottom of the list; becoming visible triggers the next page.
  const sentinelRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const id = setTimeout(() => setDebounced(query.trim()), 250);
    return () => clearTimeout(id);
  }, [query]);

  // fetchPage guards against stale responses: the list is replaced wholesale by
  // a newer search while a page is in flight, so only apply a page whose term
  // is still the current one.
  const fetchPage = useCallback(async (q: string, cursor: string) => {
    const page = await listSessions(cursor, q);
    if (queryRef.current !== q) return null;
    return page;
  }, []);

  // refresh reloads from the first page (bumped by refreshToken, e.g. after a
  // new session is created, or by the search box). Any previously loaded pages
  // are dropped so the list reflects the new activity order / search; bumping
  // the generation also voids any loadMore page still in flight.
  const refresh = useCallback(
    async (q: string) => {
      seqRef.current += 1;
      const seq = seqRef.current;
      setLoading(true);
      const page = await fetchPage(q, "");
      setLoading(false);
      // A newer refresh bumped the generation while this one was in flight
      // (e.g. the current session was deleted mid-refresh): its first page is
      // the pre-delete list, so applying it would resurrect the deleted
      // session. Drop it — the newer refresh owns the list now.
      if (seqRef.current !== seq) return;
      if (page) {
        setSessions(page.sessions);
        setNextCursor(page.nextCursor);
        setLoadError(false);
      } else {
        // The request failed: keep the current list but surface the error so a
        // transient outage is not mistaken for "no conversations yet".
        setLoadError(true);
      }
    },
    [fetchPage],
  );

  const loadMore = useCallback(async () => {
    if (!nextCursor || loadingMore) return;
    const seq = seqRef.current;
    setLoadingMore(true);
    const page = await fetchPage(debounced, nextCursor);
    setLoadingMore(false);
    if (!page) return;
    // A refresh bumped the generation while this page was in flight: its
    // cursor pages the pre-refresh list, so appending would resurrect deleted
    // sessions (or duplicate the refreshed first page). Drop it.
    if (seqRef.current !== seq) return;
    if (page.sessions.length === 0) {
      setNextCursor("");
      return;
    }
    setSessions((prev) => [...prev, ...page.sessions]);
    setNextCursor(page.nextCursor);
  }, [nextCursor, loadingMore, debounced, fetchPage]);

  useEffect(() => {
    queryRef.current = debounced;
    void refresh(debounced);
  }, [debounced, refresh, refreshToken]);

  // Infinite scroll: when the bottom sentinel scrolls into view (rootMargin
  // preloads a screenful) and more pages exist, fetch the next one.
  useEffect(() => {
    const el = sentinelRef.current;
    if (!el) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) void loadMore();
      },
      { rootMargin: "240px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [loadMore]);

  const handleDelete = async (id: string) => {
    // Deleting hides the whole conversation; confirm before acting (follows
    // the window.confirm pattern the editors use for destructive actions).
    if (!window.confirm(t("chat.deleteConfirm"))) return;
    // Cancel any in-flight run first: the server also cancels on delete, but
    // the client-side cancel keeps the run from streaming into a deleting UI
    // and covers gateways that predate the server-side cancel. Best-effort.
    await cancelSession(id);
    if (!(await deleteSession(id))) {
      reportNotice(t("chat.deleteFailed"));
      return;
    }
    if (id === currentId) {
      onDeleteCurrent();
    } else {
      void refresh(debounced);
    }
  };

  // The list is already the server's answer: the search box re-fetches with q,
  // so (unlike a client-side filter) old pages are searchable too. The search
  // box stays visible after a search with no hits, so it can be changed.
  const showSearch = sessions.length > 0 || query.trim() !== "";

  return (
    <aside className="flex h-full w-64 flex-col border-r border-border bg-muted/50">
      <div className="space-y-2 border-b border-border p-3">
        <Button size="lg" className="w-full" onClick={onNew}>
          <Plus />
          {t("chat.new")}
        </Button>
        {showSearch && (
          <InputGroup className="bg-background">
            <InputGroupAddon>
              <Search />
            </InputGroupAddon>
            <InputGroupInput
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={t("chat.searchChats")}
              aria-label={t("chat.searchChats")}
            />
          </InputGroup>
        )}
      </div>

      <ScrollArea className="min-h-0 flex-1">
        <div className="p-2">
          {loadError && (
            <div className="mb-1 flex items-center justify-between gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive">
              <span>{t("chat.loadError")}</span>
              <Button
                variant="ghost"
                size="sm"
                className="h-auto px-2 py-0.5"
                onClick={() => void refresh(debounced)}
              >
                {t("chat.retry")}
              </Button>
            </div>
          )}
          {/* The error banner and the empty states are mutually exclusive: a
              failed load must not read as "no conversations yet", so the
              empty states yield to loadError. */}
          {!loading && !loadError && sessions.length === 0 && debounced === "" && (
            <Empty className="p-4">
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <MessageSquare />
                </EmptyMedia>
                <EmptyTitle>{t("chat.noConversations")}</EmptyTitle>
                <EmptyDescription>{t("chat.noConversationsHint")}</EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
          {!loading && !loadError && sessions.length === 0 && debounced !== "" && (
            <Empty className="p-4">
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <Search />
                </EmptyMedia>
                <EmptyTitle>{t("chat.noMatches")}</EmptyTitle>
                {/* The debounced term is what the backend actually searched:
                    the live query would lag the real search during the
                    debounce window. */}
                <EmptyDescription>
                  {t("chat.noMatchesHint", { term: debounced })}
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
          {loading && sessions.length > 0 && (
            <div className="flex items-center justify-center gap-1.5 py-2 text-xs text-muted-foreground">
              <Loader2 className="size-3.5 animate-spin" />
              {t("chat.searching")}
            </div>
          )}
          <ul className="flex flex-col gap-0.5">
            {sessions.map((s) => {
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
                          {s.title || t("chat.untitled")}
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
                    aria-label={t("chat.deleteConversation")}
                    title={t("chat.deleteConversation")}
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
          <div ref={sentinelRef} className="h-px" aria-hidden="true" />
          {loadingMore && (
            <div className="flex items-center justify-center gap-1.5 py-2 text-xs text-muted-foreground">
              <Loader2 className="size-3.5 animate-spin" />
              {t("chat.loadingMore")}
            </div>
          )}
        </div>
      </ScrollArea>
    </aside>
  );
};
