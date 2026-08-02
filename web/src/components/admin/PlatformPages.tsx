// Platform administration: accounts, all teams, platform-wide usage, and
// any-scope memories. Every page here sits behind the platform-admin guard on
// the server; the console simply does not offer them to other accounts.

import { useState } from "react";
import { Link } from "react-router-dom";
import { KeyRound, Plus, Search, Trash2 } from "lucide-react";
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
import { Switch } from "@/components/ui/switch";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  adminDeleteMemory,
  adminDeprecateMemory,
  adminMemories,
  createTeamForOwner,
  createUser,
  deleteUser,
  listAllTeams,
  listUsers,
  patchUser,
  platformUsage,
  resetUserPassword,
  type User,
} from "@/lib/admin";
import { useConsoleMe } from "@/components/admin/AdminLayout";
import {
  AsyncSection,
  ErrorNotice,
  formatDate,
  PageHeader,
  PlatformRoleBadge,
  TokenStats,
  useAsync,
} from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";
import { MemoryTable, UsageRowsTable } from "@/components/admin/SelfPages";
import {
  ApproximationNotice,
  DateRangePicker,
  UsageTrend,
  useDateRange,
} from "@/components/admin/usage-parts";

const PAGE_SIZE = 25;

export function PlatformUsersPage() {
  const { me } = useConsoleMe();
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const [offset, setOffset] = useState(0);
  const state = useAsync(
    () => listUsers({ q: search, limit: PAGE_SIZE, offset }),
    [search, offset],
  );
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

  const submitSearch = (e: React.FormEvent) => {
    e.preventDefault();
    setOffset(0);
    setSearch(query.trim());
  };

  return (
    <>
      <PageHeader
        title="Users"
        description="Every account on the platform. Disabling an account revokes its tokens immediately; deleting one removes its sessions and memberships."
        actions={<CreateUserDialog onCreated={state.reload} />}
      />

      <form onSubmit={submitSearch} className="flex gap-2">
        <Input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search by email or display name"
          className="max-w-sm"
        />
        <Button type="submit" variant="outline" size="sm">
          <Search />
          Search
        </Button>
      </form>

      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading accounts">
        {(data) => (
          <div className="space-y-4">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Account</TableHead>
                  <TableHead className="w-40">Platform role</TableHead>
                  <TableHead className="w-28">Enabled</TableHead>
                  <TableHead className="w-28">Joined</TableHead>
                  <TableHead className="w-32" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.users.map((u) => (
                  <UserRow
                    key={u.id}
                    user={u}
                    isSelf={u.id === me.user.id}
                    onAct={act}
                  />
                ))}
              </TableBody>
            </Table>

            <div className="flex items-center justify-between text-sm text-muted-foreground">
              <span>
                {data.total === 0
                  ? "No accounts match."
                  : `${offset + 1}–${Math.min(offset + PAGE_SIZE, data.total)} of ${data.total}`}
              </span>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={offset === 0}
                  onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
                >
                  Previous
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={offset + PAGE_SIZE >= data.total}
                  onClick={() => setOffset(offset + PAGE_SIZE)}
                >
                  Next
                </Button>
              </div>
            </div>
          </div>
        )}
      </AsyncSection>
    </>
  );
}

function UserRow({
  user,
  isSelf,
  onAct,
}: {
  user: User;
  isSelf: boolean;
  onAct: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  return (
    <TableRow className={user.disabled ? "opacity-60" : undefined}>
      <TableCell>
        <div className="text-sm font-medium">
          {user.display_name || user.email}
          {isSelf && (
            <Badge variant="secondary" className="ml-2">
              You
            </Badge>
          )}
        </div>
        <div className="text-xs text-muted-foreground">{user.email}</div>
      </TableCell>
      <TableCell>
        {isSelf ? (
          // An administrator cannot revoke their own role — the server refuses
          // it, so the control is not offered either.
          <div className="flex items-center gap-1.5">
            <PlatformRoleBadge role={user.platform_role} />
          </div>
        ) : (
          <NativeSelect
            size="sm"
            value={user.platform_role}
            onChange={(e) =>
              onAct(() =>
                patchUser(user.id, {
                  platform_role: e.target.value as "user" | "admin",
                }),
              )
            }
            aria-label={`Platform role for ${user.email}`}
          >
            <NativeSelectOption value="user">User</NativeSelectOption>
            <NativeSelectOption value="admin">Administrator</NativeSelectOption>
          </NativeSelect>
        )}
      </TableCell>
      <TableCell>
        <Switch
          checked={!user.disabled}
          disabled={isSelf}
          onCheckedChange={(checked: boolean) =>
            onAct(() => patchUser(user.id, { disabled: !checked }))
          }
          aria-label={`Enable ${user.email}`}
        />
      </TableCell>
      <TableCell className="text-sm text-muted-foreground">
        {formatDate(user.created_at)}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-1">
          <ResetPasswordDialog user={user} onAct={onAct} />
          {!isSelf && (
            <ConfirmButton
              title={`Delete ${user.email}?`}
              description="The account, its sessions, conversations, and memberships are removed permanently. Disabling instead keeps the data and blocks sign-in."
              confirmLabel="Delete account"
              onConfirm={() => onAct(() => deleteUser(user.id))}
              trigger={
                <Button variant="ghost" size="icon-sm" aria-label="Delete account">
                  <Trash2 />
                </Button>
              }
            />
          )}
        </div>
      </TableCell>
    </TableRow>
  );
}

function ResetPasswordDialog({
  user,
  onAct,
}: {
  user: User;
  onAct: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const [password, setPassword] = useState("");

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label="Reset password">
            <KeyRound />
          </Button>
        }
      />
      <DialogContent>
        <form
          onSubmit={async (e) => {
            e.preventDefault();
            await onAct(() => resetUserPassword(user.id, password));
            setOpen(false);
            setPassword("");
          }}
        >
          <DialogHeader>
            <DialogTitle>Reset password</DialogTitle>
            <DialogDescription>
              Sets a new password for {user.email} and signs out every device
              they have. Tell them the new password over a channel you trust.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5 py-4">
            <Label htmlFor="reset-pw">New password</Label>
            <Input
              id="reset-pw"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="off"
              autoFocus
            />
            <p className="text-xs text-muted-foreground">At least 8 characters.</p>
          </div>
          <DialogFooter>
            <Button type="submit" disabled={password.length < 8}>
              Reset
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function CreateUserDialog({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await createUser({
        email: email.trim(),
        password,
        display_name: name.trim() || undefined,
      });
      setOpen(false);
      setEmail("");
      setPassword("");
      setName("");
      onCreated();
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
            <Plus />
            New account
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>Create an account</DialogTitle>
            <DialogDescription>
              There is no invitation email — set an initial password and pass it
              on yourself. The holder can change it from their profile.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="new-email">Email</Label>
              <Input
                id="new-email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="new-name">Display name</Label>
              <Input
                id="new-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Defaults to the email address"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="new-password">Initial password</Label>
              <Input
                id="new-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="off"
              />
              <p className="text-xs text-muted-foreground">At least 8 characters.</p>
            </div>
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || !email.trim() || password.length < 8}>
              {busy ? "Creating…" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function PlatformTeamsPage() {
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const state = useAsync(() => listAllTeams({ q: search, limit: 100 }), [search]);
  const [error, setError] = useState<string | null>(null);

  return (
    <>
      <PageHeader
        title="Teams"
        description="Every team on the platform. You can open any of them, whether or not you are a member."
        actions={
          <CreateTeamForOwnerDialog
            onCreated={state.reload}
            onError={setError}
          />
        }
      />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setSearch(query.trim());
        }}
        className="flex gap-2"
      >
        <Input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search by name"
          className="max-w-sm"
        />
        <Button type="submit" variant="outline" size="sm">
          <Search />
          Search
        </Button>
      </form>
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading teams">
        {(data) => (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Team</TableHead>
                <TableHead className="w-28">Members</TableHead>
                <TableHead className="w-32">Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.teams.map((t) => (
                <TableRow key={t.id}>
                  <TableCell>
                    <Link
                      to={`/admin/teams/${t.id}`}
                      className="text-sm font-medium hover:underline"
                    >
                      {t.name}
                    </Link>
                  </TableCell>
                  <TableCell className="tabular-nums">{t.members ?? 0}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {formatDate(t.created_at)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </AsyncSection>
    </>
  );
}

function CreateTeamForOwnerDialog({
  onCreated,
  onError,
}: {
  onCreated: () => void;
  onError: (msg: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [ownerId, setOwnerId] = useState("");

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button size="sm">
            <Plus />
            New team
          </Button>
        }
      />
      <DialogContent>
        <form
          onSubmit={async (e) => {
            e.preventDefault();
            try {
              await createTeamForOwner(name.trim(), ownerId.trim() || undefined);
              setOpen(false);
              setName("");
              setOwnerId("");
              onCreated();
            } catch (err) {
              onError((err as Error).message);
            }
          }}
        >
          <DialogHeader>
            <DialogTitle>Create a team</DialogTitle>
            <DialogDescription>
              Leave the owner blank to own it yourself.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="pt-name">Name</Label>
              <Input
                id="pt-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="pt-owner">Owner account id</Label>
              <Input
                id="pt-owner"
                value={ownerId}
                onChange={(e) => setOwnerId(e.target.value)}
                placeholder="Optional — defaults to you"
              />
            </div>
          </div>
          <DialogFooter>
            <Button type="submit" disabled={name.trim() === ""}>
              Create
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function PlatformUsagePage() {
  const { range, setRange } = useDateRange();
  const [groupBy, setGroupBy] = useState<"user" | "team">("user");
  const state = useAsync(
    () => platformUsage({ ...range, group_by: groupBy }),
    [range.from, range.to, groupBy],
  );

  return (
    <>
      <PageHeader
        title="Platform usage"
        description="Tokens consumed across every account. Counts only — runs do not record which model produced them, so there is no cost figure."
        actions={
          <div className="flex items-center gap-2">
            <NativeSelect
              size="sm"
              value={groupBy}
              onChange={(e) => setGroupBy(e.target.value as "user" | "team")}
              aria-label="Group by"
            >
              <NativeSelectOption value="user">By account</NativeSelectOption>
              <NativeSelectOption value="team">By team</NativeSelectOption>
            </NativeSelect>
            <DateRangePicker range={range} onChange={setRange} />
          </div>
        }
      />
      <AsyncSection state={state} loadingLabel="Loading usage">
        {(data) => (
          <div className="space-y-6">
            <ApproximationNotice note={data.note} />
            <TokenStats tokens={data.total} />
            <UsageTrend rows={data.daily} />
            <UsageRowsTable
              rows={data.rows ?? []}
              groupLabel={groupBy === "team" ? "Team" : "Account"}
            />
          </div>
        )}
      </AsyncSection>
    </>
  );
}

export function PlatformMemoriesPage() {
  const [scope, setScope] = useState<"system" | "user" | "team">("system");
  const [ownerId, setOwnerId] = useState("");
  const [applied, setApplied] = useState<{ scope: typeof scope; id: string }>({
    scope: "system",
    id: "",
  });
  const state = useAsync(
    () =>
      adminMemories({
        scope: applied.scope,
        user_id: applied.scope === "user" ? applied.id : undefined,
        team_id: applied.scope === "team" ? applied.id : undefined,
      }),
    [applied.scope, applied.id],
  );
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
    <>
      <PageHeader
        title="Memories"
        description="Long-term memory in any scope. Scope is explicit rather than inferred, so an accidental query cannot sweep every account's private memories into one view."
      />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setApplied({ scope, id: ownerId.trim() });
        }}
        className="flex flex-wrap items-end gap-2"
      >
        <div className="space-y-1.5">
          <Label htmlFor="scope">Scope</Label>
          <NativeSelect
            id="scope"
            value={scope}
            onChange={(e) => setScope(e.target.value as typeof scope)}
          >
            <NativeSelectOption value="system">System</NativeSelectOption>
            <NativeSelectOption value="user">Account</NativeSelectOption>
            <NativeSelectOption value="team">Team</NativeSelectOption>
          </NativeSelect>
        </div>
        {scope !== "system" && (
          <div className="space-y-1.5">
            <Label htmlFor="owner-id">
              {scope === "user" ? "Account id" : "Team id"}
            </Label>
            <Input
              id="owner-id"
              value={ownerId}
              onChange={(e) => setOwnerId(e.target.value)}
              className="w-80 font-mono text-sm"
            />
          </div>
        )}
        <Button type="submit" variant="outline" size="sm">
          Show
        </Button>
      </form>
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading memories">
        {(data) => (
          <MemoryTable
            memories={data.memories}
            emptyMessage="No memories in this scope."
            onDelete={(m) => act(() => adminDeleteMemory(m.id))}
            onDeprecate={(m) => act(() => adminDeprecateMemory(m.id))}
          />
        )}
      </AsyncSection>
    </>
  );
}
