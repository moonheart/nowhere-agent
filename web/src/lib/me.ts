// The signed-in account's profile, shared across the console (admin-console).
// It backs navigation gating (platform sections are hidden from ordinary
// accounts) and the team list in the sidebar.
//
// Gating here is presentation only. Every platform route enforces the role on
// the server, so a hidden menu is a courtesy, not a control.

import { useCallback, useEffect, useState } from "react";
import { getMe, type Me } from "@/lib/admin";

export type MeState = {
  me: Me | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
};

export function useMe(): MeState {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Bumping this refetches — used after a rename or a team is created, so the
  // sidebar reflects the change without a page reload.
  const [version, setVersion] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    getMe()
      .then((data) => {
        if (!cancelled) {
          setMe(data);
          setError(null);
        }
      })
      .catch((e: Error) => {
        if (!cancelled) setError(e.message);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [version]);

  const reload = useCallback(() => setVersion((v) => v + 1), []);
  return { me, loading, error, reload };
}

export const isAdmin = (me: Me | null): boolean =>
  me?.user.platform_role === "admin";

// canManageTeam reports whether the account may act on a team beyond reading:
// owner or admin in that team, or a platform administrator.
export function canManageTeam(me: Me | null, teamId: string): boolean {
  if (!me) return false;
  if (isAdmin(me)) return true;
  const t = me.teams.find((x) => x.id === teamId);
  return t?.role === "owner" || t?.role === "admin";
}

export function isTeamOwner(me: Me | null, teamId: string): boolean {
  if (!me) return false;
  if (isAdmin(me)) return true;
  return me.teams.find((x) => x.id === teamId)?.role === "owner";
}
