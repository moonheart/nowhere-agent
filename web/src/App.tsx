import { lazy, useEffect, useState } from "react";
import { Link, Route, Routes, useSearchParams } from "react-router-dom";
import { AssistantRuntimeProvider, useThread } from "@assistant-ui/react";
import { useDataStreamRuntime } from "@assistant-ui/react-data-stream";
import { LogOut, PanelLeft, PanelLeftClose, Settings } from "lucide-react";
import { AdminLayout } from "@/components/admin/AdminLayout";
import { Thread } from "@/components/thread";
import { LoginForm } from "@/components/login";
import { SessionList } from "@/components/SessionList";
import { RightPanel } from "@/components/right-panel";
import { Button, buttonVariants } from "@/components/ui/button";
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Toaster } from "@/components/ui/toast";
import { Command, CommandDialog, CommandInput, CommandList, CommandEmpty, CommandGroup, CommandItem, CommandShortcut } from "@/components/ui/command";
import { cn } from "@/lib/utils";
import { getToken, logout, consumeSSORedirect } from "@/lib/auth";
import { getSessionId, setSessionId, clearSessionId } from "@/lib/thread";
import { threadHistory, attachStream, hasActiveRun, followBody, reportMissingSession } from "@/lib/history";
import { resetActivity, reportSubagentActivity, activityEpoch, type SubagentSignal } from "@/lib/activity";
import { reportInteraction, resetApprovals, registerDecisionFollower, hasPendingInteractions, approvalEpoch, type Interaction } from "@/lib/approval";
import { clearNotice, reportNotice } from "@/lib/notice";
import { clientToolDeclarations } from "@/lib/client-tools";
import { reportPlan, resetPlan, planFromSessionState, planFromMetadata } from "@/lib/plan";
import { takeImages, addImage, resetImages, type PendingImage } from "@/lib/image-attachment";
import { selectedModel } from "@/lib/models";
import {
  resetPermissionMode,
  reportPermissionMode,
  permissionModeFromSessionState,
  pendingDraftPermissionMode,
  setPermissionMode,
} from "@/lib/permission";
import { cancelSession } from "@/lib/sessions";
import { t, useLang, setLang } from "@/lib/i18n";

// The admin console pages load on demand (React.lazy): the chat first paint
// must not download the whole console (Vite chunk >500kB warning). Each page
// is its own chunk; AdminLayout wraps its Outlet in the Suspense fallback.
const ProfilePage = lazy(() => import("@/components/admin/ProfilePage").then((m) => ({ default: m.ProfilePage })));
const MyMemoriesPage = lazy(() => import("@/components/admin/SelfPages").then((m) => ({ default: m.MyMemoriesPage })));
const MyUsagePage = lazy(() => import("@/components/admin/SelfPages").then((m) => ({ default: m.MyUsagePage })));
const TeamsPage = lazy(() => import("@/components/admin/TeamsPage").then((m) => ({ default: m.TeamsPage })));
const TeamDetailPage = lazy(() => import("@/components/admin/TeamDetailPage").then((m) => ({ default: m.TeamDetailPage })));
const PlatformMemoriesPage = lazy(() => import("@/components/admin/PlatformPages").then((m) => ({ default: m.PlatformMemoriesPage })));
const PlatformTeamsPage = lazy(() => import("@/components/admin/PlatformPages").then((m) => ({ default: m.PlatformTeamsPage })));
const PlatformUsagePage = lazy(() => import("@/components/admin/PlatformPages").then((m) => ({ default: m.PlatformUsagePage })));
const PlatformUsersPage = lazy(() => import("@/components/admin/PlatformPages").then((m) => ({ default: m.PlatformUsersPage })));
const MySkillsPage = lazy(() => import("@/components/admin/SkillsPages").then((m) => ({ default: m.MySkillsPage })));
const PlatformSkillsPage = lazy(() => import("@/components/admin/SkillsPages").then((m) => ({ default: m.PlatformSkillsPage })));
const MyAgentDefsPage = lazy(() => import("@/components/admin/AgentDefsPages").then((m) => ({ default: m.MyAgentDefsPage })));
const PlatformAgentDefsPage = lazy(() => import("@/components/admin/AgentDefsPages").then((m) => ({ default: m.PlatformAgentDefsPage })));
const PlatformAuditPage = lazy(() => import("@/components/admin/AuditPage").then((m) => ({ default: m.PlatformAuditPage })));
const PlatformSettingsPage = lazy(() => import("@/components/admin/SettingsPage").then((m) => ({ default: m.PlatformSettingsPage })));
const PlatformProvidersPage = lazy(() => import("@/components/admin/PlatformProvidersPage").then((m) => ({ default: m.PlatformProvidersPage })));
const PlatformQuotasPage = lazy(() => import("@/components/admin/QuotasPage").then((m) => ({ default: m.PlatformQuotasPage })));
const ScheduledTasksPage = lazy(() => import("@/components/admin/ScheduledTasksPage").then((m) => ({ default: m.ScheduledTasksPage })));

// Chat holds one conversation: remounting it (via React key) resets the runtime
// and re-runs history.load() for the now-current sessionId.
function Chat({
  conversationKey,
  sessionId,
  onSession,
}: {
  conversationKey: number;
  sessionId: string | null;
  onSession: (id: string) => void;
}) {
  // Captured once per mounted conversation. Subagent signals stream from the
  // backend; if a late frame arrives after a reset/switch, the store drops it.
  const chatEpoch = activityEpoch();
  // Interaction frames are tagged with the APPROVAL store's epoch — the same
  // counter the verdict/attach streams in lib/history.ts report against — so
  // all interaction-frame reporters share one reset counter instead of two
  // that only stay in sync because every reset site bumps both.
  const interactionEpoch = approvalEpoch();
  // Images staged for the CURRENT send: the body closure takes the batch from
  // the attachment store (clearing the chips), and a FAILED send puts it back
  // so the chips — and the batch — survive a retry. Success keeps them cleared
  // (the images rode the accepted turn).
  let sendImages: PendingImage[] = [];
  // Set when a send is refused with 404: the backend refused the explicitly
  // named thread (missing/foreign), reportMissingSession was already fired and
  // the turn's onError must not add a second, generic notice.
  let sessionMissing = false;
  const runtime = useDataStreamRuntime({
    api: "/api/chat",
    headers: async (): Promise<Record<string, string>> => {
      const token = getToken();
      return token ? { authorization: `Bearer ${token}` } : {};
    },
    onResponse: (res) => {
      // The send carried an explicit threadId; a 404 means that session does
      // not exist or belongs to someone else, so the backend refuses to
      // silently create a fresh session under the stale id. Clear it (the
      // session:missing listener resets the conversation) instead of letting
      // the failed turn linger in a blank thread.
      if (res.status === 404) {
        sessionMissing = true;
        reportMissingSession();
      }
    },
    body: async () => {
      const threadId = getSessionId();
      // Declare the browser's client-side tools (clipboard, timezone, …) to the
      // agent on every turn. The model may call them; the backend suspends the
      // run and streams a client_tool interaction the browser fulfils locally.
      const tools = clientToolDeclarations();
      // Images the composer staged for THIS turn (uploaded first, paths held in
      // the attachment store). Consuming them here clears the chips — the batch
      // is exactly what this send carries. Kept in sendImages so onError can
      // restore the chips when the send fails.
      // Reset at the start of every send: a turn without images must not keep
      // the previous turn's batch staged, or a later failure would restore
      // stale images into the composer.
      sendImages = [];
      const images = takeImages();
      if (images.length > 0) sendImages = images;
      // The composer's model picker: the chosen model rides this turn's body
      // ("" = the server's resolved default). A stale picker name is harmless —
      // the backend falls back to the default rather than failing the run.
      const model = selectedModel();
      return {
        ...(threadId ? { threadId } : {}),
        ...(model ? { model } : {}),
        ...(Object.keys(tools).length > 0 ? { tools } : {}),
        ...(images.length > 0
          ? { images: images.map((p) => ({ path: p.path, mediaType: p.mediaType })) }
          : {}),
      };
    },
    onData: (d) => {
      if (d.name === "session") {
        const id = (d.data as { id?: string })?.id;
        if (id) {
          setSessionId(id);
          onSession(id);
        }
      } else if (d.name === "subagent") {
        // Live subagent progress (from a spawn_agent tool call) — feed the
        // right panel's Runs tab; transient, never part of the message.
        reportSubagentActivity(d.data as SubagentSignal, chatEpoch);
      } else if (d.name === "interaction" || d.name === "tool-approval") {
        // A tool call is parked awaiting the client (general interrupt): a
        // dangerous-action approval, an ask_user question set, or a client_tool
        // the browser auto-executes. Show the matching card on the tool call;
        // transient, not the message. ("tool-approval" is the legacy frame name.)
        // Tagged with the conversation epoch so a late frame from a session
        // that was reset meanwhile can't land in the new conversation.
        reportInteraction(d.data as Interaction, interactionEpoch);
      } else if (d.name === "session-state") {
        // Session-level state push (O1): the plan_write tool's plan, pushed
        // live. Feeds the top plan panel. The permission_mode setting rides the
        // same channel — report whichever key the frame carries.
        const plan = planFromSessionState(d.data);
        if (plan) reportPlan(plan);
        const mode = permissionModeFromSessionState(d.data);
        if (mode !== null) reportPermissionMode(mode);
      }
    },
    // The Stop button only aborts the local fetch; also tell the backend to
    // cancel the run so the model + sandbox stop, not just the HTTP stream.
    onCancel: () => {
      const id = getSessionId();
      if (id) void cancelSession(id);
    },
    adapters: { history: threadHistory },
    onError: (e) => {
      // The session-missing case was already handled in onResponse (a 404 on
      // an explicitly named thread); the run then fails with the same status
      // and must not add a second, misleading notice on top.
      if (sessionMissing) {
        sessionMissing = false;
        return;
      }
      // A failed send consumed the staged batch from the body closure (the
      // chips were cleared when the request body was built); put them back so
      // retrying the turn still carries the images — a rejected send must not
      // silently drop the attachments. addImage dedupes by path, so restoring
      // is safe against a body closure that ran more than once.
      if (sendImages.length > 0) {
        for (const img of sendImages) addImage(img);
        sendImages = [];
      }
      // A send rejected by the pending-interaction gate (409): point at the
      // parked card instead of a bare console error. The typed error body
      // doesn't reach this callback, so detect the condition client-side — a
      // failed send while cards hang is this gate in practice. The rejected
      // message stays in the thread, so nothing typed is lost.
      if (hasPendingInteractions()) {
        reportNotice(t("chat.pendingNotice"));
        return;
      }
      // Any other send failure (4xx/5xx, network): surface it in the UI, not
      // just the console — a silent submit drop reads as "the message was
      // sent". The failed turn stays in the thread, so it can be retried.
      console.error("chat error", e);
      reportNotice(t("chat.sendFailedNotice"));
    },
  });

  // A decided approval/ask_user starts a FRESH run on the backend (run-stateless
  // model) and returns its SSE stream. Follow it here so the deciding client
  // watches the continuation live.
  //
  // parentId = the current last message (the gated assistant turn). The new
  // assistant message MUST be its child (U→M1→M3), NOT a root sibling
  // (parentId:null): MessageRepository.resetHead HARD-DELETES the displaced
  // subtree when head moves off a branch, so a root-level sibling makes the whole
  // U→M1 chain an orphaned branch and wipes it off the canvas until a history
  // reload. As a child of M1 it extends the same branch and nothing is pruned.
  useEffect(() => {
    registerDecisionFollower((stream) => {
      const msgs = runtime.thread.getState().messages;
      const parentId = msgs.length > 0 ? msgs[msgs.length - 1].id : null;
      void runtime.thread.resumeRun({
        parentId,
        stream: () => followBody(stream),
      });
    });
  }, [runtime]);

  // Multi-client attach (design D13): while this client is idle, poll the
  // session to notice a run started on another tab/device and live-follow it
  // via resumeRun. The follow re-streams the whole run and renders ONE new
  // assistant message; the runtime aborts any prior stream before a new one, so
  // a poll attach racing the load+resume follow just re-follows (aborting the
  // stale stream) rather than duplicating — the resume stream replaces the
  // same new message's content each time.
  //
  // The poll cadence backs off as the session stays idle (2s → 5s → 60s) and
  // snaps back to the fast tier on any activity (a run detected, or a local run
  // finishing): an idle tab must not hammer the backend every 2 seconds
  // forever, but must notice a new remote run promptly. The 60s cap keeps a
  // permanently idle tab at one /active poll per minute — each poll is two
  // DB reads on the backend (authorizeSession + the durable ActiveRun
  // fallback), so the cap bounds that per-tab idle traffic.
  useEffect(() => {
    let cancelled = false;
    let attaching = false;
    let idleStreak = 0;
    let timer: number | undefined;
    const intervalFor = (streak: number) =>
      streak < 2 ? 2000 : streak < 6 ? 5000 : 60000;
    const schedule = () => {
      timer = window.setTimeout(tick, intervalFor(idleStreak));
    };
    const tick = async () => {
      if (cancelled) return;
      if (attaching || runtime.thread.getState().isRunning) {
        // A run is active locally (or an attach is in flight): no point polling
        // — but keep the fast cadence, and reset the idle streak, so the
        // moment the session goes idle again a remote run is noticed promptly.
        idleStreak = 0;
        timer = window.setTimeout(tick, 2000);
        return;
      }
      const threadId = getSessionId();
      if (!threadId) {
        idleStreak = Math.max(2, idleStreak + 1);
        schedule();
        return;
      }
      let active = false;
      try {
        active = await hasActiveRun();
      } catch {
        schedule(); // transient failure; retry at the current cadence
        return;
      }
      if (cancelled) return;
      if (!active) {
        idleStreak++;
        schedule();
        return;
      }
      idleStreak = 0;
      if (runtime.thread.getState().isRunning) {
        schedule();
        return;
      }
      attaching = true;
      try {
        // Attach the re-streamed run as a child of the current head, NOT at the
        // root (parentId:null). A root-level sibling makes the existing
        // user→assistant branch an orphan, and resetHead then HARD-DELETES it —
        // that was the "messages vanish after approve" bug: the poll fired while
        // this client was idle (verdict run active on the backend, follow not yet
        // started) and wiped the user's question + gated turn off the canvas.
        // Appending after head keeps the whole chain on one branch; the runtime's
        // stream-abort dedupes a decision-follow racing this same run.
        const msgs = runtime.thread.getState().messages;
        const parentId = msgs.length > 0 ? msgs[msgs.length - 1].id : null;
        await runtime.thread.resumeRun({ parentId, stream: attachStream });
      } catch {
        // Attach is best-effort; a failed follow is retried next tick.
      } finally {
        attaching = false;
        if (!cancelled) schedule();
      }
    };
    schedule();
    return () => {
      cancelled = true;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [runtime]);

  return (
    <AssistantRuntimeProvider runtime={runtime} key={conversationKey}>
      <Thread sessionId={sessionId} />
      <PlanMetadataWatcher />
    </AssistantRuntimeProvider>
  );
}

// PlanMetadataWatcher feeds the plan store from the message metadata, the path a
// NON-transient data-session-state frame takes on the direct-submit flow (which
// has no onData callback). useThread subscribes reactively, so the panel updates
// live as the run streams and stays correct across a history reload. Must render
// inside AssistantRuntimeProvider.
function PlanMetadataWatcher() {
  const plan = useThread((s) => {
    for (let i = s.messages.length - 1; i >= 0; i--) {
      const p = planFromMetadata(s.messages[i].metadata);
      if (p) return p;
    }
    return null;
  });
  useEffect(() => {
    if (plan) reportPlan(plan);
  }, [plan]);
  return null;
}

// ChatApp is the conversation view. Authentication now lives in App, which
// gates both this and the console, so ChatApp only reports the sign-out.
// LangToggle flips the UI language between 中文 and English. The label shows
// the language you switch TO (the current language's name, per convention).
function LangToggle() {
  const lang = useLang();
  return (
    <Button
      variant="ghost"
      size="sm"
      title={lang === "zh" ? t("lang.switchToEn") : t("lang.switchToZh")}
      onClick={() => setLang(lang === "zh" ? "en" : "zh")}
    >
      {lang === "zh" ? "EN" : "中文"}
    </Button>
  );
}

function ChatApp({ onSignedOut }: { onSignedOut: () => void }) {
  const [conversationKey, setConversationKey] = useState(0);
  const [leftCollapsed, setLeftCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem("nowhere.leftCollapsed") === "1";
    } catch {
      return false;
    }
  });
  useEffect(() => {
    try {
      localStorage.setItem("nowhere.leftCollapsed", leftCollapsed ? "1" : "0");
    } catch {
      // ignore quota
    }
  }, [leftCollapsed]);
  const [commandOpen, setCommandOpen] = useState(false);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setCommandOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  // The URL is the shareable source of truth for the active conversation: the
  // `session` query carries its id, so a reload/deep-link reopens it and the
  // address bar always reflects what you're chatting in.
  const [searchParams, setSearchParams] = useSearchParams();
  const urlSessionId = searchParams.get("session");
  const [activeSessionId, setActiveSessionId] = useState<string | null>(() =>
    urlSessionId ?? getSessionId(),
  );
  // Bumped whenever a session is created/switched so the sidebar refetches.
  const [listVersion, setListVersion] = useState(0);

  // The backend refused an explicitly named thread (404 "session not found" /
  // 403 "forbidden" — see reportMissingSession in lib/history): the stored id
  // is stale (a shared link whose session was deleted, or someone else's
  // session). Clear it from localStorage and the URL, drop the notice banner
  // and remount a fresh thread, so the receiver lands in a new conversation
  // instead of a blank one that would then overwrite the stored id on the
  // first message.
  useEffect(() => {
    const onMissing = () => {
      clearSessionId();
      setActiveSessionId(null);
      reportNotice(t("chat.missingSession"));
      setConversationKey((k) => k + 1);
    };
    window.addEventListener("session:missing", onMissing);
    return () => window.removeEventListener("session:missing", onMissing);
  }, []);

  // Open the conversation the URL points at. `replace` keeps session hops out of
  // the history stack, so Back leaves the chat rather than cycling prior
  // conversations; delete/empty URLs fall back to the stored id. (initialUrlId
  // is captured on mount: the URL's own updates would otherwise retrigger this.)
  const [initialUrlId] = useState(urlSessionId);
  useEffect(() => {
    if (initialUrlId && initialUrlId !== activeSessionId) {
      setSessionId(initialUrlId);
      setActiveSessionId(initialUrlId);
      resetActivity();
      resetApprovals();
      resetPlan();
      resetPermissionMode();
      resetImages();
      clearNotice();
      setConversationKey((k) => k + 1);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Sync the URL's `session` query with the active id (or drop it for a fresh
  // chat). `replace` again keeps this off the Back-button trail.
  useEffect(() => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (activeSessionId) next.set("session", activeSessionId);
        else next.delete("session");
        return next;
      },
      { replace: true },
    );
  }, [activeSessionId, setSearchParams]);

  const startNewChat = () => {
    clearSessionId();
    setActiveSessionId(null);
    resetActivity();
    resetApprovals();
    resetPlan();
    resetImages();
    clearNotice();
    // Do NOT resetPermissionMode here: a draft 完全允许 picked on this blank
    // thread is a one-off for the session the first message creates; clearing it
    // now would discard that choice. Once the session exists its own mode takes
    // over, and allow_all stays scoped to that one session.
    setConversationKey((k) => k + 1);
  };

  // Deleting the active session resets to a fresh thread and refreshes the list.
  const handleDeleteCurrent = () => {
    startNewChat();
    setListVersion((v) => v + 1);
  };

  const switchTo = (id: string) => {
    if (id === activeSessionId) return;
    setSessionId(id);
    setActiveSessionId(id);
    resetActivity();
    resetApprovals();
    resetPlan();
    resetPermissionMode();
    resetImages();
    clearNotice();
    setConversationKey((k) => k + 1);
  };

  // Called when a brand-new session is created server-side (first message of a
  // new chat): adopt it as active and refresh the sidebar. The runtime is
  // already streaming into this session, so no remount is needed. If the user
  // picked 完全允许 on the blank draft, apply that one-off choice to THIS new
  // session (it is not a persistent default for future chats).
  const handleNewSession = (id: string) => {
    if (pendingDraftPermissionMode() === "allow_all") {
      void setPermissionMode(id, "allow_all");
    }
    setActiveSessionId((prev) => {
      if (prev === id) return prev;
      setListVersion((v) => v + 1);
      return id;
    });
  };

  return (
    <div className="flex h-dvh flex-col bg-background text-foreground">
      <header className="flex h-12 items-center gap-3 border-b border-border px-4">
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label={leftCollapsed ? "展开侧边栏" : "收起侧边栏"}
          title={leftCollapsed ? "展开侧边栏" : "收起侧边栏"}
          onClick={() => setLeftCollapsed((v) => !v)}
          className="shrink-0"
        >
          {leftCollapsed ? <PanelLeft /> : <PanelLeftClose />}
        </Button>
        <div className="flex items-center gap-2">
          <span className="flex size-6 items-center justify-center rounded-lg bg-primary text-xs font-bold text-primary-foreground">
            n
          </span>
          <span className="text-sm font-semibold tracking-tight">
            nowhere-agent
          </span>
        </div>
        <div className="ml-auto flex items-center gap-1">
          {/* UI language toggle: a user override (nowhere.lang) beats the
              browser locale; the header re-renders via useLang, other t()
              consumers pick the new language up on their next render. */}
          <LangToggle />
          {/* buttonVariants, not <Button render={<Link/>}>: base-ui's Button
              assumes a native <button>, and telling it otherwise costs the
              anchor its link semantics (middle-click, open in new tab). */}
          <Link
            to="/admin"
            title="Settings and administration"
            className={cn(buttonVariants({ variant: "ghost", size: "sm" }))}
          >
            <Settings />
            Console
          </Link>
          <Button
            variant="ghost"
            size="sm"
            title="Sign out"
            onClick={() => {
              clearSessionId();
              void logout().finally(onSignedOut);
            }}
          >
            <LogOut />
            Sign out
          </Button>
        </div>
      </header>
      <div className="flex min-h-0 flex-1">
        {!leftCollapsed && (
          <div className="w-64 shrink-0">
            <SessionList
              currentId={activeSessionId}
              onSelect={switchTo}
              onNew={startNewChat}
              onDeleteCurrent={handleDeleteCurrent}
              refreshToken={listVersion}
            />
          </div>
        )}
        <ResizablePanelGroup orientation="horizontal" className="min-h-0 flex-1">
        <ResizablePanel defaultSize={72} minSize={40} id="center">
          <main className="flex h-full min-w-0 flex-1 flex-col bg-background">
            {/* key on Chat itself (not just the provider inside it): switching /
                starting a chat must rebuild the WHOLE Chat, including its runtime.
                Without it, Chat survives the switch and keeps the previous
                conversation's runtime — the remounted provider's cards then render
                that stale runtime's old messages under the NEW epoch and re-report
                them into the right panel, leaking the prior chat's files into a
                fresh conversation's Workspace. A fresh runtime starts empty and
                reloads only the now-current session's history. */}
            <Chat key={conversationKey} conversationKey={conversationKey} sessionId={activeSessionId} onSession={handleNewSession} />
          </main>
        </ResizablePanel>
        <ResizableHandle withHandle />
        <ResizablePanel defaultSize={24} minSize={16} maxSize={40} collapsible collapsedSize={0}>
          <RightPanel />
        </ResizablePanel>
      </ResizablePanelGroup>
      <CommandDialog open={commandOpen} onOpenChange={setCommandOpen}>
        <Command>
          <CommandInput placeholder="搜索会话或执行指令…" />
          <CommandList>
            <CommandEmpty>无结果</CommandEmpty>
            <CommandGroup heading="操作">
              <CommandItem
                onSelect={() => {
                  setCommandOpen(false);
                  startNewChat();
                }}
              >
                新建对话 <CommandShortcut>⌘N</CommandShortcut>
              </CommandItem>
              <CommandItem
                onSelect={() => {
                  setCommandOpen(false);
                  setLeftCollapsed((v) => !v);
                }}
              >
                {leftCollapsed ? "展开侧边栏" : "收起侧边栏"}
              </CommandItem>
              <CommandItem
                onSelect={() => {
                  setCommandOpen(false);
                  window.location.href = "/admin";
                }}
              >
                前往控制台
              </CommandItem>
            </CommandGroup>
            <CommandGroup heading="会话">
              <CommandItem
                onSelect={() => {
                  setCommandOpen(false);
                  if (activeSessionId) switchTo(activeSessionId);
                }}
              >
                当前会话
                {activeSessionId && <CommandShortcut>{activeSessionId.slice(0, 6)}</CommandShortcut>}
              </CommandItem>
            </CommandGroup>
          </CommandList>
        </Command>
      </CommandDialog>
      </div>
    </div>
  );
}

// App gates everything behind a token and routes between the chat view and the
// management console. The console lives at its own path so links into it are
// shareable and the back button behaves; the Go server serves index.html for
// those paths (see spaHandler) so a deep link loads.
export default function App() {
  const [token, setToken] = useState<string | null>(() => getToken());
  // SSO hand-off (P1-2): the OIDC callback returns the browser to /#token=...
  // (or /#sso_error=...). Consume it once on mount — before the gate below — so
  // an IdP sign-in lands signed-in and an SSO failure reaches the login form.
  const [ssoError, setSsoError] = useState<string | null>(null);
  const [ssoTotp, setSsoTotp] = useState<string | null>(null);
  useEffect(() => {
    const { token: ssoToken, error, totpRequired } = consumeSSORedirect();
    if (ssoToken) setToken(ssoToken);
    if (error) setSsoError(error);
    // The IdP authenticated the account but its second factor is on: hand the
    // challenge to the login form so the user completes MFA in the browser.
    if (totpRequired) setSsoTotp(totpRequired);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  // Global 401 handling: an expired/revoked token answers 401 on any
  // authenticated request; the shared fetch helpers clear the token and
  // broadcast auth:expired, and this listener returns the app to the login
  // screen — instead of leaving the user staring at a dead session whose
  // requests all fail with generic errors.
  useEffect(() => {
    const onExpired = () => setToken(null);
    window.addEventListener("auth:expired", onExpired);
    return () => window.removeEventListener("auth:expired", onExpired);
  }, []);

  if (!token) {
    return (
      <div className="flex h-dvh flex-col bg-background text-foreground">
        <div className="min-h-0 flex-1">
          <LoginForm
            onSuccess={() => setToken(getToken())}
            ssoError={ssoError}
            initialTotpToken={ssoTotp}
          />
        </div>
      </div>
    );
  }

  return (
    <TooltipProvider>
      <Toaster>
        <Routes>
          <Route path="/" element={<ChatApp onSignedOut={() => setToken(null)} />} />
          <Route path="/admin" element={<AdminLayout />}>
            <Route index element={<ProfilePage />} />
            <Route path="usage" element={<MyUsagePage />} />
            <Route path="memories" element={<MyMemoriesPage />} />
            <Route path="skills" element={<MySkillsPage />} />
            <Route path="agents" element={<MyAgentDefsPage />} />
            <Route path="scheduled-tasks" element={<ScheduledTasksPage />} />
            <Route path="teams" element={<TeamsPage />} />
            <Route path="teams/:teamId" element={<TeamDetailPage />} />
            <Route path="platform/users" element={<PlatformUsersPage />} />
            <Route path="platform/teams" element={<PlatformTeamsPage />} />
            <Route path="platform/usage" element={<PlatformUsagePage />} />
            <Route path="platform/quotas" element={<PlatformQuotasPage />} />
            <Route path="platform/providers" element={<PlatformProvidersPage />} />
            <Route path="platform/memories" element={<PlatformMemoriesPage />} />
            <Route path="platform/skills" element={<PlatformSkillsPage />} />
            <Route path="platform/agents" element={<PlatformAgentDefsPage />} />
            <Route path="platform/audit" element={<PlatformAuditPage />} />
            <Route path="platform/settings" element={<PlatformSettingsPage />} />
          </Route>
          {/* Anything else falls back to the chat view rather than a blank page. */}
          <Route path="*" element={<ChatApp onSignedOut={() => setToken(null)} />} />
        </Routes>
      </Toaster>
    </TooltipProvider>
  );
}
