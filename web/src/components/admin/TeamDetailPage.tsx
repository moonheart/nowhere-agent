// One team: members, provider keys, usage, and memories, gated by the caller's
// role in that team. Every control here is also enforced server-side — hiding a
// button is a courtesy, not a permission.

import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { KeyRound, Trash2, UserPlus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  addMember,
  changeMemberRole,
  deleteKey,
  deleteTeam,
  deleteTeamMemory,
  deprecateTeamMemory,
  getTeam,
  listKeys,
  listMembers,
  putKey,
  removeMember,
  renameTeam,
  teamMemories,
  teamUsage,
  type Member,
  type TeamRole,
} from "@/lib/admin";
import { canManageTeam, isTeamOwner } from "@/lib/me";
import { useConsoleMe } from "@/components/admin/AdminLayout";
import {
  AsyncSection,
  ErrorNotice,
  formatDate,
  formatDateTime,
  PageHeader,
  RoleBadge,
  TokenStats,
  useAsync,
} from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";
import { MemoryTable } from "@/components/admin/SelfPages";
import {
  ApproximationNotice,
  DateRangePicker,
  UsageTrend,
  useDateRange,
} from "@/components/admin/usage-parts";

const ROLES: TeamRole[] = ["owner", "admin", "member"];

// PROVIDERS mirrors the server's allowlist. A key stored under any other name
// would never be selected on the chat path.
const PROVIDERS = ["anthropic", "openai"];

export function TeamDetailPage() {
  const { teamId = "" } = useParams();
  const { me, reload: reloadMe } = useConsoleMe();
  const navigate = useNavigate();
  const state = useAsync(() => getTeam(teamId), [teamId]);

  const manage = canManageTeam(me, teamId);
  const owner = isTeamOwner(me, teamId);

  return (
    <AsyncSection state={state} loadingLabel="Loading team">
      {(data) => (
        <>
          <PageHeader
            title={data.team.name}
            description={`Created ${formatDate(data.team.created_at)}`}
            actions={
              <div className="flex items-center gap-2">
                {manage && (
                  <RenameTeamDialog
                    teamId={teamId}
                    current={data.team.name}
                    onDone={() => {
                      state.reload();
                      reloadMe();
                    }}
                  />
                )}
                {owner && (
                  <ConfirmButton
                    title={`Delete ${data.team.name}?`}
                    description="The team, its memberships, and its provider keys are removed permanently. Team-scoped memories become unreachable."
                    confirmLabel="Delete team"
                    onConfirm={async () => {
                      await deleteTeam(teamId);
                      reloadMe();
                      navigate("/admin/teams");
                    }}
                    trigger={
                      <Button variant="destructive" size="sm">
                        Delete
                      </Button>
                    }
                  />
                )}
              </div>
            }
          />
          <Tabs defaultValue="members">
            <TabsList>
              <TabsTrigger value="members">Members</TabsTrigger>
              {manage && <TabsTrigger value="keys">Provider keys</TabsTrigger>}
              {manage && <TabsTrigger value="usage">Usage</TabsTrigger>}
              <TabsTrigger value="memories">Memories</TabsTrigger>
            </TabsList>

            <TabsContent value="members" className="pt-4">
              <MembersTab teamId={teamId} canManage={manage} canSetOwner={owner} />
            </TabsContent>
            {manage && (
              <TabsContent value="keys" className="pt-4">
                <KeysTab teamId={teamId} />
              </TabsContent>
            )}
            {manage && (
              <TabsContent value="usage" className="pt-4">
                <TeamUsageTab teamId={teamId} />
              </TabsContent>
            )}
            <TabsContent value="memories" className="pt-4">
              <TeamMemoriesTab teamId={teamId} canManage={manage} />
            </TabsContent>
          </Tabs>
        </>
      )}
    </AsyncSection>
  );
}

function RenameTeamDialog({
  teamId,
  current,
  onDone,
}: {
  teamId: string;
  current: string;
  onDone: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState(current);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await renameTeam(teamId, name.trim());
      setOpen(false);
      onDone();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            Rename
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>Rename team</DialogTitle>
            <DialogDescription>Members see the new name immediately.</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5 py-4">
            <Label htmlFor="rename">Name</Label>
            <Input id="rename" value={name} onChange={(e) => setName(e.target.value)} autoFocus />
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || name.trim() === ""}>
              {busy ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function MembersTab({
  teamId,
  canManage,
  canSetOwner,
}: {
  teamId: string;
  canManage: boolean;
  canSetOwner: boolean;
}) {
  const { me, reload: reloadMe } = useConsoleMe();
  const state = useAsync(() => listMembers(teamId), [teamId]);
  const [error, setError] = useState<string | null>(null);

  // act runs a mutation, then refreshes both the member list and the profile —
  // a role change or a departure alters the sidebar too.
  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try {
      await fn();
      state.reload();
      reloadMe();
    } catch (err) {
      setError((err as Error).message);
    }
  };
  const refresh = () => {
    state.reload();
    reloadMe();
  };

  return (
    <div className="space-y-4">
      {canManage && (
        <AddMemberDialog
          teamId={teamId}
          canSetOwner={canSetOwner}
          onAdded={refresh}
        />
      )}
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading members">
        {(data) => (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Member</TableHead>
                <TableHead className="w-40">Role</TableHead>
                <TableHead className="w-32">Joined</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.members.map((m) => (
                <TableRow key={m.user_id}>
                  <TableCell>
                    <div className="text-sm font-medium">
                      {m.display_name || m.email}
                      {m.user_id === me.user.id && (
                        <Badge variant="secondary" className="ml-2">
                          You
                        </Badge>
                      )}
                      {m.disabled && (
                        <Badge variant="destructive" className="ml-2">
                          Disabled
                        </Badge>
                      )}
                    </div>
                    <div className="text-xs text-muted-foreground">{m.email}</div>
                  </TableCell>
                  <TableCell>
                    {canSetOwner ? (
                      <NativeSelect
                        size="sm"
                        value={m.role}
                        onChange={(e) =>
                          act(() =>
                            changeMemberRole(teamId, m.user_id, e.target.value as TeamRole),
                          )
                        }
                        aria-label={`Role for ${m.email}`}
                      >
                        {ROLES.map((r) => (
                          <NativeSelectOption key={r} value={r}>
                            {r}
                          </NativeSelectOption>
                        ))}
                      </NativeSelect>
                    ) : (
                      <RoleBadge role={m.role} />
                    )}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {formatDate(m.joined_at)}
                  </TableCell>
                  <TableCell className="text-right">
                    <RemoveMemberButton
                      teamId={teamId}
                      member={m}
                      isSelf={m.user_id === me.user.id}
                      canManage={canManage}
                      onDone={refresh}
                    />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </AsyncSection>
    </div>
  );
}

function RemoveMemberButton({
  teamId,
  member,
  isSelf,
  canManage,
  onDone,
}: {
  teamId: string;
  member: Member;
  isSelf: boolean;
  canManage: boolean;
  onDone: () => void;
}) {
  // Anyone may remove themselves — that is leaving. Removing someone else needs
  // the admin role in the team.
  if (!isSelf && !canManage) return null;

  return (
    <ConfirmButton
      title={isSelf ? "Leave this team?" : `Remove ${member.email}?`}
      description={
        isSelf
          ? "You lose access to the team's shared memories, skills, and provider keys."
          : "They lose access to the team's shared memories, skills, and provider keys. Their own data is untouched."
      }
      confirmLabel={isSelf ? "Leave" : "Remove"}
      onConfirm={async () => {
        await removeMember(teamId, member.user_id);
        onDone();
      }}
      trigger={
        <Button variant="ghost" size="sm">
          {isSelf ? "Leave" : "Remove"}
        </Button>
      }
    />
  );
}

function AddMemberDialog({
  teamId,
  canSetOwner,
  onAdded,
}: {
  teamId: string;
  canSetOwner: boolean;
  onAdded: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<TeamRole>("member");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await addMember(teamId, email.trim(), role);
      setOpen(false);
      setEmail("");
      onAdded();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const roles = canSetOwner ? ROLES : ROLES.filter((r) => r !== "owner");

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button size="sm">
            <UserPlus />
            Add member
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>Add a member</DialogTitle>
            <DialogDescription>
              The person must already have an account — there is no invitation email.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="member-email">Email</Label>
              <Input
                id="member-email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="teammate@example.com"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="member-role">Role</Label>
              <NativeSelect
                id="member-role"
                value={role}
                onChange={(e) => setRole(e.target.value as TeamRole)}
              >
                {roles.map((r) => (
                  <NativeSelectOption key={r} value={r}>
                    {r}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
              {!canSetOwner && (
                <p className="text-xs text-muted-foreground">
                  Only an owner can add another owner.
                </p>
              )}
            </div>
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || email.trim() === ""}>
              {busy ? "Adding…" : "Add"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function KeysTab({ teamId }: { teamId: string }) {
  const state = useAsync(() => listKeys(teamId), [teamId]);
  const [error, setError] = useState<string | null>(null);

  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try {
      await fn();
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        A key configured here is used for this team's members' model calls instead
        of the platform key. Keys are write-only: once saved, only the last four
        characters are ever shown again.
      </p>
      <SetKeyDialog teamId={teamId} onSaved={state.reload} />
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading keys">
        {(data) =>
          data.keys.length === 0 ? (
            <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
              No provider keys configured — this team's calls use the platform key.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Provider</TableHead>
                  <TableHead>Key</TableHead>
                  <TableHead className="w-40">Last updated</TableHead>
                  <TableHead className="w-16" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.keys.map((k) => (
                  <TableRow key={k.provider}>
                    <TableCell className="font-medium capitalize">{k.provider}</TableCell>
                    <TableCell className="font-mono text-sm text-muted-foreground">
                      {k.masked}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {formatDateTime(k.updated_at)}
                    </TableCell>
                    <TableCell className="text-right">
                      <ConfirmButton
                        title={`Remove the ${k.provider} key?`}
                        description="This team's calls fall back to the platform key."
                        confirmLabel="Remove"
                        onConfirm={() => act(() => deleteKey(teamId, k.provider))}
                        trigger={
                          <Button variant="ghost" size="icon-sm" aria-label="Remove key">
                            <Trash2 />
                          </Button>
                        }
                      />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )
        }
      </AsyncSection>
    </div>
  );
}

function SetKeyDialog({ teamId, onSaved }: { teamId: string; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [provider, setProvider] = useState(PROVIDERS[0]);
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await putKey(teamId, provider, key.trim());
      setOpen(false);
      setKey("");
      onSaved();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button size="sm">
            <KeyRound />
            Set a key
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>Set a provider key</DialogTitle>
            <DialogDescription>
              Saving over an existing provider rotates the key. It takes effect on
              the team's next model call.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="key-provider">Provider</Label>
              <NativeSelect
                id="key-provider"
                value={provider}
                onChange={(e) => setProvider(e.target.value)}
              >
                {PROVIDERS.map((p) => (
                  <NativeSelectOption key={p} value={p}>
                    {p}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="key-value">API key</Label>
              <Input
                id="key-value"
                type="password"
                value={key}
                onChange={(e) => setKey(e.target.value)}
                placeholder="sk-…"
                autoComplete="off"
              />
            </div>
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || key.trim() === ""}>
              {busy ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function TeamUsageTab({ teamId }: { teamId: string }) {
  const { range, setRange } = useDateRange();
  const state = useAsync(() => teamUsage(teamId, range), [teamId, range.from, range.to]);

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <DateRangePicker range={range} onChange={setRange} />
      </div>
      <AsyncSection state={state} loadingLabel="Loading usage">
        {(data) => (
          <div className="space-y-4">
            <ApproximationNotice note={data.note} />
            <TokenStats tokens={data.total} />
            <UsageTrend rows={data.daily} />
          </div>
        )}
      </AsyncSection>
    </div>
  );
}

function TeamMemoriesTab({ teamId, canManage }: { teamId: string; canManage: boolean }) {
  const state = useAsync(() => teamMemories(teamId), [teamId]);
  const [error, setError] = useState<string | null>(null);

  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try {
      await fn();
      state.reload();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  return (
    <div className="space-y-4">
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading memories">
        {(data) => (
          <MemoryTable
            memories={data.memories}
            emptyMessage="Nothing shared yet. Team memories accumulate as the dreaming worker consolidates members' sessions."
            readOnly={!canManage}
            onDelete={canManage ? (m) => act(() => deleteTeamMemory(teamId, m.id)) : undefined}
            onDeprecate={
              canManage ? (m) => act(() => deprecateTeamMemory(teamId, m.id)) : undefined
            }
          />
        )}
      </AsyncSection>
    </div>
  );
}
