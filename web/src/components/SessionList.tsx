import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronDown, ChevronRight, Loader2, MoreHorizontal, Pencil, Pin, PinOff, Plus, Search, Trash2 } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Separator } from "@/components/ui/separator";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu";
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
  ItemTitle,
} from "@/components/ui/item";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { t, useLang } from "@/lib/i18n";
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

const PIN_STORAGE_KEY = "nowhere.pinnedSessions";
const TITLE_OVERRIDE_KEY = "nowhere.sessionTitleOverrides";
const PINNED_COLLAPSED_KEY = "nowhere.pinnedCollapsed";
const RECENT_COLLAPSED_KEY = "nowhere.recentCollapsed";

function loadPinned(): Set<string> {
  try {
    const raw = localStorage.getItem(PIN_STORAGE_KEY);
    if (!raw) return new Set();
    const arr = JSON.parse(raw) as unknown;
    if (!Array.isArray(arr)) return new Set();
    return new Set(arr.filter((x): x is string => typeof x === "string"));
  } catch {
    return new Set();
  }
}

function savePinned(ids: Set<string>) {
  try {
    localStorage.setItem(PIN_STORAGE_KEY, JSON.stringify([...ids]));
  } catch {
    // ignore quota
  }
}

function loadTitleOverrides(): Record<string, string> {
  try {
    const raw = localStorage.getItem(TITLE_OVERRIDE_KEY);
    if (!raw) return {};
    const obj = JSON.parse(raw) as unknown;
    if (!obj || typeof obj !== "object") return {};
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(obj as Record<string, unknown>)) {
      if (typeof v === "string" && v.trim()) out[k] = v;
    }
    return out;
  } catch {
    return {};
  }
}

function saveTitleOverrides(map: Record<string, string>) {
  try {
    localStorage.setItem(TITLE_OVERRIDE_KEY, JSON.stringify(map));
  } catch {
    // ignore quota
  }
}

// SessionList is the left sidebar of conversations. Selecting one switches the
// active thread; "New chat" starts a fresh session; the menu per row offers
// rename / pin / delete.
export const SessionList = ({ currentId, onSelect, onNew, onDeleteCurrent, refreshToken }: Props) => {
  const lang = useLang();
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

  // Local UI state: pinned set + title overrides (persisted in localStorage
  // until a real PATCH /api/chat/sessions/{id} endpoint exists).
  const [pinnedIds, setPinnedIds] = useState<Set<string>>(() => loadPinned());
  const [titleOverrides, setTitleOverrides] = useState<Record<string, string>>(() => loadTitleOverrides());
  const [renameTarget, setRenameTarget] = useState<SessionSummary | null>(null);
  const [renameDraft, setRenameDraft] = useState("");
  const [pinnedCollapsed, setPinnedCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem(PINNED_COLLAPSED_KEY) === "1";
    } catch {
      return false;
    }
  });
  const [recentCollapsed, setRecentCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem(RECENT_COLLAPSED_KEY) === "1";
    } catch {
      return false;
    }
  });
  useEffect(() => {
    try {
      localStorage.setItem(PINNED_COLLAPSED_KEY, pinnedCollapsed ? "1" : "0");
    } catch {
      // ignore
    }
  }, [pinnedCollapsed]);
  useEffect(() => {
    try {
      localStorage.setItem(RECENT_COLLAPSED_KEY, recentCollapsed ? "1" : "0");
    } catch {
      // ignore
    }
  }, [recentCollapsed]);

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
    if (recentCollapsed) return;
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
  }, [nextCursor, loadingMore, debounced, fetchPage, recentCollapsed]);

  useEffect(() => {
    queryRef.current = debounced;
    void refresh(debounced);
  }, [debounced, refresh, refreshToken]);

  // Infinite scroll: when the bottom sentinel scrolls into view (rootMargin
  // preloads a screenful) and more pages exist, fetch the next one.
  // Paused while "最近" is collapsed — otherwise the collapsed height makes
  // the sentinel immediately intersect (240px margin) and pages all sessions.
  useEffect(() => {
    if (recentCollapsed) return;
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
  }, [loadMore, recentCollapsed]);

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
    // also drop local overrides/pins for the deleted session
    setPinnedIds((prev) => {
      if (!prev.has(id)) return prev;
      const next = new Set(prev);
      next.delete(id);
      savePinned(next);
      return next;
    });
    setTitleOverrides((prev) => {
      if (!(id in prev)) return prev;
      const next = { ...prev };
      delete next[id];
      saveTitleOverrides(next);
      return next;
    });
  };

  const togglePin = useCallback((id: string) => {
    setPinnedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      savePinned(next);
      return next;
    });
  }, []);

  const openRename = useCallback((s: SessionSummary) => {
    const cur = titleOverrides[s.id] ?? s.title ?? "";
    setRenameTarget(s);
    setRenameDraft(cur);
  }, [titleOverrides]);

  const commitRename = useCallback(() => {
    if (!renameTarget) return;
    const id = renameTarget.id;
    const nextTitle = renameDraft.trim();
    if (!nextTitle) {
      reportNotice(t("chat.renameFailed"));
      return;
    }
    // Optimistic local update — persists across refreshes until backend supports rename.
    // When a PATCH endpoint is added, call it here and fall back to local on 404.
    setSessions((prev) => prev.map((x) => (x.id === id ? { ...x, title: nextTitle } : x)));
    setTitleOverrides((prev) => {
      const next = { ...prev, [id]: nextTitle };
      saveTitleOverrides(next);
      return next;
    });
    setRenameTarget(null);
  }, [renameTarget, renameDraft]);

  // The list is already the server's answer: the search box re-fetches with q,
  // so (unlike a client-side filter) old pages are searchable too. The search
  // box stays visible after a search with no hits, so it can be changed.
  const showSearch = sessions.length > 0 || query.trim() !== "";

  const withDisplayTitle = sessions.map((s) => ({
    raw: s,
    displayTitle: titleOverrides[s.id] ?? s.title,
  }));
  // pinned first, preserving server order within each group
  const pinned = withDisplayTitle.filter((x) => pinnedIds.has(x.raw.id));
  const recent = withDisplayTitle.filter((x) => !pinnedIds.has(x.raw.id));

  return (
    <aside className="flex h-full w-full flex-col border-r border-border bg-muted/50">
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
                  <Search />
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
          {/* Pinned / Recent separated, each collapsible */}
          {(() => {
            const renderRow = ({ raw: s, displayTitle }: { raw: SessionSummary; displayTitle: string }) => {
              const active = s.id === currentId;
              const isPinned = pinnedIds.has(s.id);
              const timeLabel = relTime(s.updatedAt, lang);
              const fullTime = (() => {
                try {
                  return new Date(s.updatedAt).toLocaleString(lang === "zh" ? "zh-CN" : "en-US", {
                    year: "numeric",
                    month: "2-digit",
                    day: "2-digit",
                    hour: "2-digit",
                    minute: "2-digit",
                  });
                } catch {
                  return timeLabel;
                }
              })();
              return (
                <li key={s.id} className="group relative list-none">
                  <ContextMenu>
                    <ContextMenuTrigger>
                      <Item
                        size="xs"
                        className={cn(
                          "cursor-pointer gap-2 pr-2 text-left font-normal",
                          active ? "bg-primary/10 text-foreground" : "text-foreground hover:bg-muted",
                        )}
                        render={<button type="button" onClick={() => onSelect(s.id)} />}
                      >
                    <ItemContent className="min-w-0 gap-0">
                      <ItemTitle className="w-full min-w-0">
                        <span className="min-w-0 flex-1 truncate">{displayTitle || t("chat.untitled")}</span>
                      </ItemTitle>
                    </ItemContent>
                      </Item>
                    </ContextMenuTrigger>
                    <ContextMenuContent className="w-40">
                      <ContextMenuItem onClick={() => openRename(s)}>
                        <Pencil /> {t("chat.rename")}
                      </ContextMenuItem>
                      <ContextMenuItem onClick={() => togglePin(s.id)}>
                        {isPinned ? <PinOff /> : <Pin />} {isPinned ? t("chat.unpin") : t("chat.pin")}
                      </ContextMenuItem>
                      <ContextMenuSeparator />
                      <ContextMenuItem variant="destructive" onClick={() => void handleDelete(s.id)}>
                        <Trash2 /> {t("chat.deleteConversation")}
                      </ContextMenuItem>
                    </ContextMenuContent>
                  </ContextMenu>
                  <div
                    className={cn(
                      "pointer-events-none absolute top-1/2 right-1 flex -translate-y-1/2 items-center gap-0.5 rounded-md bg-muted/90 px-0.5 py-0.5 backdrop-blur-sm transition-opacity",
                      "opacity-0 group-hover:pointer-events-auto group-hover:opacity-100 group-focus-within:pointer-events-auto group-focus-within:opacity-100 has-[[data-state=open]]:pointer-events-auto has-[[data-state=open]]:opacity-100",
                      active && "bg-primary/10",
                    )}
                    onClick={(e) => e.stopPropagation()}
                    onPointerDown={(e) => e.stopPropagation()}
                  >
                    <Tooltip>
                      <TooltipTrigger
                        render={
                          <span className="max-w-[72px] truncate px-1 text-xs whitespace-nowrap text-muted-foreground">
                            {timeLabel}
                          </span>
                        }
                      />
                      <TooltipContent side="top">{fullTime}</TooltipContent>
                    </Tooltip>
                    <Button
                      variant="ghost"
                      size="icon-xs"
                      aria-label={isPinned ? t("chat.unpin") : t("chat.pin")}
                      title={isPinned ? t("chat.unpin") : t("chat.pin")}
                      onClick={(e) => {
                        e.stopPropagation();
                        togglePin(s.id);
                      }}
                      className={cn("size-6 shrink-0 text-muted-foreground hover:bg-accent hover:text-foreground", isPinned && "text-primary")}
                    >
                      {isPinned ? <PinOff className="size-3.5" /> : <Pin className="size-3.5" />}
                    </Button>
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        render={
                          <Button
                            variant="ghost"
                            size="icon-xs"
                            aria-label={t("chat.moreActions")}
                            title={t("chat.moreActions")}
                            className="size-6 shrink-0 text-muted-foreground hover:bg-accent hover:text-foreground"
                          />
                        }
                      >
                        <MoreHorizontal className="size-3.5" />
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="start" side="bottom" sideOffset={6} alignOffset={-4} className="w-40">
                        <DropdownMenuItem
                          onClick={(e) => {
                            e.stopPropagation();
                            openRename(s);
                          }}
                        >
                          <Pencil />
                          {t("chat.rename")}
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onClick={(e) => {
                            e.stopPropagation();
                            togglePin(s.id);
                          }}
                        >
                          {isPinned ? <PinOff /> : <Pin />}
                          {isPinned ? t("chat.unpin") : t("chat.pin")}
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          variant="destructive"
                          onClick={(e) => {
                            e.stopPropagation();
                            void handleDelete(s.id);
                          }}
                        >
                          <Trash2 />
                          {t("chat.deleteConversation")}
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </div>
                </li>
              );
            };
            return (
              <div className="flex flex-col gap-3">
                {/* Pinned section */}
                <div>
                  <button
                    type="button"
                    onClick={() => setPinnedCollapsed((v) => !v)}
                    className="flex w-full items-center gap-1 rounded px-1 py-1 text-xs font-medium text-muted-foreground hover:bg-muted hover:text-foreground"
                    aria-expanded={!pinnedCollapsed}
                  >
                    {pinnedCollapsed ? <ChevronRight className="size-3.5" /> : <ChevronDown className="size-3.5" />}
                    <span>{t("chat.pinned")}</span>
                    <span className="ml-1 rounded bg-muted px-1 py-0.5 text-[10px] leading-none">{pinned.length}</span>
                  </button>
                  {!pinnedCollapsed && (
                    <>
                      {pinned.length > 0 ? (
                        <ul className="mt-1 flex flex-col gap-px">{pinned.map(renderRow)}</ul>
                      ) : (
                        <div className="px-2 py-1 text-xs text-muted-foreground/70">{t("chat.noPinned")}</div>
                      )}
                    </>
                  )}
                </div>
                <Separator />
                {/* Recent section */}
                <div>
                  <button
                    type="button"
                    onClick={() => setRecentCollapsed((v) => !v)}
                    className="flex w-full items-center gap-1 rounded px-1 py-1 text-xs font-medium text-muted-foreground hover:bg-muted hover:text-foreground"
                    aria-expanded={!recentCollapsed}
                  >
                    {recentCollapsed ? <ChevronRight className="size-3.5" /> : <ChevronDown className="size-3.5" />}
                    <span>{t("chat.recent")}</span>
                    <span className="ml-1 rounded bg-muted px-1 py-0.5 text-[10px] leading-none">{recent.length}</span>
                  </button>
                  {!recentCollapsed && <ul className="mt-1 flex flex-col gap-px">{recent.map(renderRow)}</ul>}
                </div>
              </div>
            );
          })()}
          <div ref={sentinelRef} className="h-px" aria-hidden="true" />
          {loadingMore && (
            <div className="flex items-center justify-center gap-1.5 py-2 text-xs text-muted-foreground">
              <Loader2 className="size-3.5 animate-spin" />
              {t("chat.loadingMore")}
            </div>
          )}
        </div>
      </ScrollArea>

      <Dialog open={!!renameTarget} onOpenChange={(open) => (!open ? setRenameTarget(null) : null)}>
        <DialogContent className="sm:max-w-sm" onClick={(e) => e.stopPropagation()}>
          <DialogHeader>
            <DialogTitle>{t("chat.renameTitle")}</DialogTitle>
            <DialogDescription className="sr-only">{t("chat.renameTitle")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2 py-1">
            <Label htmlFor="rename-input" className="sr-only">
              {t("chat.rename")}
            </Label>
            <Input
              id="rename-input"
              value={renameDraft}
              onChange={(e) => setRenameDraft(e.target.value)}
              placeholder={t("chat.renamePlaceholder")}
              autoFocus
              onKeyDown={(e) => {
                if (e.key === "Enter") commitRename();
                if (e.key === "Escape") setRenameTarget(null);
              }}
            />
          </div>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setRenameTarget(null)}>
              {t("chat.renameCancel")}
            </Button>
            <Button onClick={commitRename} disabled={!renameDraft.trim()}>
              {t("chat.renameConfirm")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </aside>
  );
};
