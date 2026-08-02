// The console shell: a nav rail plus the routed page (admin-console).
//
// Navigation is gated by role — platform sections only appear for platform
// administrators, and each team appears with the caller's role in it. That is
// presentation: every route is enforced server-side regardless of what renders.

import { Link, NavLink, Outlet, useLocation } from "react-router-dom";
import {
  ArrowLeft,
  Brain,
  Building2,
  ChartNoAxesColumn,
  Loader2,
  ShieldCheck,
  UserRound,
  Users,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Separator } from "@/components/ui/separator";
import { isAdmin, useMe } from "@/lib/me";
import { ErrorNotice } from "@/components/admin/common";
import { cn } from "@/lib/utils";
import type { Me } from "@/lib/admin";
import { createContext, useContext, type ReactNode } from "react";

// MeContext hands the loaded profile to pages so each one does not refetch it.
// It is only provided below a successful load, so consumers never see null.
const MeContext = createContext<{ me: Me; reload: () => void } | null>(null);

export function useConsoleMe() {
  const ctx = useContext(MeContext);
  if (!ctx) {
    throw new Error("useConsoleMe must be used inside AdminLayout");
  }
  return ctx;
}

function NavItem({
  to,
  icon,
  children,
  end,
}: {
  to: string;
  icon: ReactNode;
  children: ReactNode;
  end?: boolean;
}) {
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        cn(
          "flex items-center gap-2 rounded-lg px-2.5 py-1.5 text-sm transition-colors",
          isActive
            ? "bg-muted font-medium text-foreground"
            : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
        )
      }
    >
      <span className="[&_svg]:size-4">{icon}</span>
      <span className="truncate">{children}</span>
    </NavLink>
  );
}

function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div className="px-2.5 pt-4 pb-1 text-xs font-medium tracking-wide text-muted-foreground uppercase">
      {children}
    </div>
  );
}

export function AdminLayout() {
  const { me, loading, error, reload } = useMe();
  const location = useLocation();

  if (loading) {
    return (
      <div className="flex h-dvh items-center justify-center text-sm text-muted-foreground">
        <Loader2 className="mr-2 size-4 animate-spin" />
        Loading console…
      </div>
    );
  }
  if (error || !me) {
    return (
      <div className="mx-auto max-w-lg p-8">
        <ErrorNotice message={error ?? "could not load your profile"} />
        <Link
          to="/"
          className="mt-4 inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft className="size-4" />
          Back to chat
        </Link>
      </div>
    );
  }

  const admin = isAdmin(me);

  return (
    <MeContext.Provider value={{ me, reload }}>
      <div className="console-scope flex h-dvh bg-background text-foreground">
        <aside className="flex w-60 shrink-0 flex-col border-r border-border bg-card">
          <div className="flex h-12 items-center gap-2 px-4">
            <span className="flex size-6 items-center justify-center rounded-lg bg-primary text-xs font-bold text-primary-foreground">
              n
            </span>
            <span className="text-sm font-semibold tracking-tight">Console</span>
          </div>
          <Separator />

          <nav className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
            <SectionLabel>Account</SectionLabel>
            <NavItem to="/admin" end icon={<UserRound />}>
              Profile
            </NavItem>
            <NavItem to="/admin/usage" icon={<ChartNoAxesColumn />}>
              My usage
            </NavItem>
            <NavItem to="/admin/memories" icon={<Brain />}>
              My memories
            </NavItem>

            <SectionLabel>Teams</SectionLabel>
            <NavItem to="/admin/teams" end icon={<Building2 />}>
              All my teams
            </NavItem>
            {me.teams.map((t) => (
              <NavLink
                key={t.id}
                to={`/admin/teams/${t.id}`}
                className={({ isActive }) =>
                  cn(
                    "flex items-center justify-between gap-2 rounded-lg px-2.5 py-1.5 text-sm transition-colors",
                    isActive
                      ? "bg-muted font-medium text-foreground"
                      : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
                  )
                }
              >
                <span className="truncate pl-6">{t.name}</span>
                <Badge variant="outline" className="shrink-0 capitalize">
                  {t.role}
                </Badge>
              </NavLink>
            ))}

            {admin && (
              <>
                <SectionLabel>Platform</SectionLabel>
                <NavItem to="/admin/platform/users" icon={<Users />}>
                  Users
                </NavItem>
                <NavItem to="/admin/platform/teams" icon={<Building2 />}>
                  Teams
                </NavItem>
                <NavItem to="/admin/platform/usage" icon={<ChartNoAxesColumn />}>
                  Usage
                </NavItem>
                <NavItem to="/admin/platform/memories" icon={<Brain />}>
                  Memories
                </NavItem>
              </>
            )}
          </nav>

          <Separator />
          <div className="space-y-2 p-3">
            <div className="flex items-center gap-2 px-1">
              {admin && <ShieldCheck className="size-3.5 shrink-0 text-primary" />}
              <span className="truncate text-xs text-muted-foreground">
                {me.user.display_name || me.user.email}
              </span>
            </div>
            <Link
              to="/"
              className="flex items-center gap-2 rounded-lg px-2.5 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-muted/60 hover:text-foreground"
            >
              <ArrowLeft className="size-4" />
              Back to chat
            </Link>
          </div>
        </aside>

        <main
          className="min-w-0 flex-1 overflow-y-auto"
          // Remount on navigation so a page's local state (filters, forms)
          // does not leak into the next one.
          key={location.pathname}
        >
          <div className="mx-auto max-w-5xl space-y-6 p-6">
            <Outlet />
          </div>
        </main>
      </div>
    </MeContext.Provider>
  );
}
