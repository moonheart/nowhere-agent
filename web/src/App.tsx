import { useEffect, useState } from "react";
import { Link, Route, Routes } from "react-router-dom";
import { AssistantRuntimeProvider, useThread } from "@assistant-ui/react";
import { useDataStreamRuntime } from "@assistant-ui/react-data-stream";
import { LogOut, Settings } from "lucide-react";
import { AdminLayout } from "@/components/admin/AdminLayout";
import { ProfilePage } from "@/components/admin/ProfilePage";
import { MyMemoriesPage, MyUsagePage } from "@/components/admin/SelfPages";
import { TeamsPage } from "@/components/admin/TeamsPage";
import { TeamDetailPage } from "@/components/admin/TeamDetailPage";
import {
  PlatformMemoriesPage,
  PlatformTeamsPage,
  PlatformUsagePage,
  PlatformUsersPage,
} from "@/components/admin/PlatformPages";
import { Thread } from "@/components/thread";
import { LoginForm } from "@/components/login";
import { SessionList } from "@/components/SessionList";
import { RightPanel } from "@/components/right-panel";
import { Button, buttonVariants } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { getToken, logout } from "@/lib/auth";
import { getSessionId, setSessionId, clearSessionId } from "@/lib/thread";
import { threadHistory, attachStream, hasActiveRun, followBody } from "@/lib/history";
import { resetActivity, reportSubagentActivity, type SubagentSignal } from "@/lib/activity";
import { reportInteraction, resetApprovals, registerDecisionFollower, type Interaction } from "@/lib/approval";
import { clientToolDeclarations } from "@/lib/client-tools";
import { reportPlan, resetPlan, planFromSessionState, planFromMetadata } from "@/lib/plan";
import {
  resetPermissionMode,
  reportPermissionMode,
  permissionModeFromSessionState,
  pendingDraftPermissionMode,
  setPermissionMode,
} from "@/lib/permission";
import { cancelSession } from "@/lib/sessions";

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
  const runtime = useDataStreamRuntime({
    api: "/api/chat",
    headers: async (): Promise<Record<string, string>> => {
      const token = getToken();
      return token ? { authorization: `Bearer ${token}` } : {};
    },
    body: async () => {
      const threadId = getSessionId();
      // Declare the browser's client-side tools (clipboard, timezone, …) to the
      // agent on every turn. The model may call them; the backend suspends the
      // run and streams a client_tool interaction the browser fulfils locally.
      const tools = clientToolDeclarations();
      return {
        ...(threadId ? { threadId } : {}),
        ...(Object.keys(tools).length > 0 ? { tools } : {}),
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
        reportSubagentActivity(d.data as SubagentSignal);
      } else if (d.name === "interaction" || d.name === "tool-approval") {
        // A tool call is parked awaiting the client (general interrupt): a
        // dangerous-action approval, an ask_user question set, or a client_tool
        // the browser auto-executes. Show the matching card on the tool call;
        // transient, not the message. ("tool-approval" is the legacy frame name.)
        reportInteraction(d.data as Interaction);
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
    onError: (e) => console.error("chat error", e),
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
  useEffect(() => {
    let cancelled = false;
    let attaching = false;
    const tick = async () => {
      if (cancelled || attaching) return;
      const threadId = getSessionId();
      if (!threadId) return;
      if (runtime.thread.getState().isRunning) return;
      let active = false;
      try {
        active = await hasActiveRun();
      } catch {
        return; // transient failure; try again next tick
      }
      if (cancelled || !active || runtime.thread.getState().isRunning) return;
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
      }
    };
    const id = setInterval(tick, 2000);
    return () => {
      cancelled = true;
      clearInterval(id);
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
function ChatApp({ onSignedOut }: { onSignedOut: () => void }) {
  const [conversationKey, setConversationKey] = useState(0);
  const [activeSessionId, setActiveSessionId] = useState<string | null>(() =>
    getSessionId(),
  );
  // Bumped whenever a session is created/switched so the sidebar refetches.
  const [listVersion, setListVersion] = useState(0);

  const startNewChat = () => {
    clearSessionId();
    setActiveSessionId(null);
    resetActivity();
    resetApprovals();
    resetPlan();
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
        <div className="flex items-center gap-2">
          <span className="flex size-6 items-center justify-center rounded-lg bg-primary text-xs font-bold text-primary-foreground">
            n
          </span>
          <span className="text-sm font-semibold tracking-tight">
            nowhere-agent
          </span>
        </div>
        <div className="ml-auto flex items-center gap-1">
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
        <SessionList
          currentId={activeSessionId}
          onSelect={switchTo}
          onNew={startNewChat}
          onDeleteCurrent={handleDeleteCurrent}
          refreshToken={listVersion}
        />
        <main className="min-w-0 flex-1 bg-background">
          <Chat conversationKey={conversationKey} sessionId={activeSessionId} onSession={handleNewSession} />
        </main>
        <RightPanel />
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

  if (!token) {
    return (
      <div className="flex h-dvh flex-col bg-background text-foreground">
        <div className="min-h-0 flex-1">
          <LoginForm onSuccess={() => setToken(getToken())} />
        </div>
      </div>
    );
  }

  return (
    <Routes>
      <Route path="/" element={<ChatApp onSignedOut={() => setToken(null)} />} />
      <Route path="/admin" element={<AdminLayout />}>
        <Route index element={<ProfilePage />} />
        <Route path="usage" element={<MyUsagePage />} />
        <Route path="memories" element={<MyMemoriesPage />} />
        <Route path="teams" element={<TeamsPage />} />
        <Route path="teams/:teamId" element={<TeamDetailPage />} />
        <Route path="platform/users" element={<PlatformUsersPage />} />
        <Route path="platform/teams" element={<PlatformTeamsPage />} />
        <Route path="platform/usage" element={<PlatformUsagePage />} />
        <Route path="platform/memories" element={<PlatformMemoriesPage />} />
      </Route>
      {/* Anything else falls back to the chat view rather than a blank page. */}
      <Route path="*" element={<ChatApp onSignedOut={() => setToken(null)} />} />
    </Routes>
  );
}
