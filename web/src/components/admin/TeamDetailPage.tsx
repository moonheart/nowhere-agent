// One team: members, provider keys, usage, and memories, gated by the caller's
// role in that team. Every control here is also enforced server-side — hiding a
// button is a courtesy, not a permission.

import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Plus, UserPlus } from "lucide-react";
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
  clearTeamAssignment,
  createTeamModel,
  createTeamProvider,
  deleteTeam,
  deleteTeamMemory,
  deleteTeamModel,
  deleteTeamProvider,
  deprecateTeamMemory,
  fetchTeamModels,
  getTeam,
  listMembers,
  listTeamProviders,
  removeMember,
  renameTeam,
  setTeamAssignment,
  setTeamDefaultModel,
  teamMemories,
  teamUsage,
  updateTeamModel,
  updateTeamProvider,
  type Member,
  type Provider,
  type ProviderModel,
  type TeamRole,
} from "@/lib/admin";
import { canManageTeam, isTeamOwner } from "@/lib/me";
import { t } from "@/lib/i18n";
import { useConsoleMe } from "@/components/admin/AdminLayout";
import {
  AsyncSection,
  ErrorNotice,
  formatDate,
  PageHeader,
  RoleBadge,
  TokenStats,
  useAsync,
} from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";
import { MemoryTable } from "@/components/admin/SelfPages";
import { SkillEditor } from "@/components/admin/SkillEditor";
import { AgentDefEditor } from "@/components/admin/AgentDefEditor";
import {
  ApproximationNotice,
  DateRangePicker,
  UsageTrend,
  useDateRange,
} from "@/components/admin/usage-parts";
import {
  FetchModelsDialog,
  ModelFormDialog,
  ProviderCard,
  ProviderFormDialog,
} from "@/components/admin/ProvidersParts";

const ROLES: TeamRole[] = ["owner", "admin", "member"];

export function TeamDetailPage() {
  const { teamId = "" } = useParams();
  const { me, reload: reloadMe } = useConsoleMe();
  const navigate = useNavigate();
  const state = useAsync(() => getTeam(teamId), [teamId]);

  const manage = canManageTeam(me, teamId);
  const owner = isTeamOwner(me, teamId);

  return (
    <AsyncSection state={state} loadingLabel={t("teamPage.loading")}>
      {(data) => (
        <>
          <PageHeader
            title={data.team.name}
            description={t("teamPage.created", { date: formatDate(data.team.created_at) })}
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
                    title={t("teamPage.deleteTitle", { name: data.team.name })}
                    description={t("teamPage.deleteDescription")}
                    confirmLabel={t("teamPage.deleteTeam")}
                    onConfirm={async () => {
                      await deleteTeam(teamId);
                      reloadMe();
                      navigate("/admin/teams");
                    }}
                    trigger={
                      <Button variant="destructive" size="sm">
                        {t("teamPage.delete")}
                      </Button>
                    }
                  />
                )}
              </div>
            }
          />
          <Tabs defaultValue="members">
            <TabsList>
              <TabsTrigger value="members">{t("teamPage.tabMembers")}</TabsTrigger>
              {manage && <TabsTrigger value="providers">{t("teamPage.tabProviders")}</TabsTrigger>}
              {manage && <TabsTrigger value="usage">{t("teamPage.tabUsage")}</TabsTrigger>}
              <TabsTrigger value="memories">{t("teamPage.tabMemories")}</TabsTrigger>
              <TabsTrigger value="skills">{t("teamPage.tabSkills")}</TabsTrigger>
              <TabsTrigger value="agents">{t("teamPage.tabAgents")}</TabsTrigger>
            </TabsList>

            <TabsContent value="members" className="pt-4">
              <MembersTab teamId={teamId} canManage={manage} canSetOwner={owner} />
            </TabsContent>
            {manage && (
              <TabsContent value="providers" className="pt-4">
                <ProvidersTab teamId={teamId} />
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
            <TabsContent value="skills" className="pt-4">
              {/* The editor sizes itself with flex-1 inside its parent's flex
                  column; a tab panel is not one, so give it an explicit height. */}
              <div className="flex h-[calc(100dvh-16rem)] flex-col">
                <SkillEditor base={{ kind: "team", teamId }} canWrite={manage} />
              </div>
            </TabsContent>
            <TabsContent value="agents" className="pt-4">
              <AgentDefEditor base={{ kind: "team", teamId }} canWrite={manage} />
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
            {t("teamPage.rename")}
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("teamPage.renameTitle")}</DialogTitle>
            <DialogDescription>{t("teamPage.renameDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5 py-4">
            <Label htmlFor="rename">{t("teamPage.name")}</Label>
            <Input id="rename" value={name} onChange={(e) => setName(e.target.value)} autoFocus />
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || name.trim() === ""}>
              {busy ? t("teamPage.saving") : t("teamPage.save")}
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
      <AsyncSection state={state} loadingLabel={t("teamPage.loadingMembers")}>
        {(data) => (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("teamPage.colMember")}</TableHead>
                <TableHead className="w-40">{t("teamPage.colRole")}</TableHead>
                <TableHead className="w-32">{t("teamPage.colJoined")}</TableHead>
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
                          {t("teamPage.you")}
                        </Badge>
                      )}
                      {m.disabled && (
                        <Badge variant="destructive" className="ml-2">
                          {t("teamPage.disabled")}
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
                        aria-label={t("teamPage.roleAria", { email: m.email })}
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
      title={isSelf ? t("teamPage.leaveTitle") : t("teamPage.removeTitle", { email: member.email })}
      description={
        isSelf ? t("teamPage.leaveDescription") : t("teamPage.removeDescription")
      }
      confirmLabel={isSelf ? t("teamPage.leave") : t("teamPage.remove")}
      onConfirm={async () => {
        await removeMember(teamId, member.user_id);
        onDone();
      }}
      trigger={
        <Button variant="ghost" size="sm">
          {isSelf ? t("teamPage.leave") : t("teamPage.remove")}
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
            {t("teamPage.addMember")}
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("teamPage.addMemberTitle")}</DialogTitle>
            <DialogDescription>{t("teamPage.addMemberDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="member-email">{t("teamPage.email")}</Label>
              <Input
                id="member-email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder={t("teamPage.emailPlaceholder")}
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="member-role">{t("teamPage.role")}</Label>
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
                  {t("teamPage.ownerOnlyHint")}
                </p>
              )}
            </div>
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || email.trim() === ""}>
              {busy ? t("teamPage.adding") : t("teamPage.add")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function ProvidersTab({ teamId }: { teamId: string }) {
  const state = useAsync(() => listTeamProviders(teamId), [teamId]);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Provider | null>(null);
  const [fetching, setFetching] = useState<Provider | null>(null);
  const [addingModelTo, setAddingModelTo] = useState<Provider | null>(null);
  const [editingModel, setEditingModel] = useState<{
    provider: Provider;
    model: ProviderModel;
  } | null>(null);

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
      <p className="text-sm text-muted-foreground">{t("teamPage.providersIntro")}</p>
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel={t("teamPage.loadingProviders")}>
        {(data) => (
          <>
            <AssignmentPicker
              teamId={teamId}
              providers={data.providers}
              assignment={data.assignment}
              onSaved={() => {
                state.reload();
              }}
            />
            <div className="flex items-center justify-between pt-2">
              <h3 className="text-sm font-medium">{t("teamPage.providersSection")}</h3>
              <ProviderFormDialog
                trigger={
                  <Button size="sm">
                    <Plus />
                    {t("teamPage.addProvider")}
                  </Button>
                }
                title={t("teamPage.addProviderTitle")}
                description={t("teamPage.addProviderDescription")}
                submitLabel={t("teamPage.addProviderSubmit")}
                onSave={(b) => createTeamProvider(teamId, b)}
                onDone={state.reload}
              />
            </div>
            {data.providers.length === 0 ? (
              <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                {t("teamPage.noProviders")}
              </p>
            ) : (
              <div className="space-y-4">
                {data.providers.map((p) => (
                  <ProviderCard
                    key={p.id}
                    provider={p}
                    canWrite={p.scope === "team"}
                    assignment={data.assignment?.provider_id === p.id}
                    onEdit={() => setEditing(p)}
                    onDelete={(pr) => act(() => deleteTeamProvider(teamId, pr.id))}
                    onFetchModels={() => setFetching(p)}
                    onAddModel={() => setAddingModelTo(p)}
                    onUpdateModel={(pr, m) =>
                      setEditingModel({ provider: pr, model: m })
                    }
                    onDeleteModel={(pr, m) =>
                      act(() => deleteTeamModel(teamId, pr.id, m.id))
                    }
                    onSetDefaultModel={(pr, m) =>
                      act(() => setTeamDefaultModel(teamId, pr.id, m.id))
                    }
                  />
                ))}
              </div>
            )}
          </>
        )}
      </AsyncSection>

      {editing && (
        <ProviderFormDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          title={t("teamPage.editProviderTitle")}
          description={t("teamPage.editProviderDescription")}
          initial={editing}
          submitLabel={t("teamPage.saveProvider")}
          onSave={(b) => updateTeamProvider(teamId, editing.id, b)}
          onDone={state.reload}
        />
      )}
      {fetching && (
        <FetchModelsDialog
          open
          onOpenChange={(open) => !open && setFetching(null)}
          fetchModels={() => fetchTeamModels(teamId, fetching.id).then((r) => r.models)}
          addModel={(name) => createTeamModel(teamId, fetching.id, { name })}
          onDone={state.reload}
        />
      )}
      {addingModelTo && (
        <ModelFormDialog
          open
          onOpenChange={(open) => !open && setAddingModelTo(null)}
          title={t("teamPage.addModelTo", { name: addingModelTo.name })}
          description={t("teamPage.modelVisionDescription")}
          onSave={(b) => createTeamModel(teamId, addingModelTo.id, b)}
          onDone={state.reload}
        />
      )}
      {editingModel && (
        <ModelFormDialog
          open
          onOpenChange={(open) => !open && setEditingModel(null)}
          title={t("teamPage.editModelTitle")}
          description={t("teamPage.editModelDescription")}
          initial={editingModel.model}
          onSave={(b) =>
            updateTeamModel(teamId, editingModel.provider.id, editingModel.model.id, b)
          }
          onDone={state.reload}
        />
      )}
    </div>
  );
}

function AssignmentPicker({
  teamId,
  providers,
  assignment,
  onSaved,
}: {
  teamId: string;
  providers: Provider[];
  assignment: { provider_id: string; model_id?: string } | null;
  onSaved: () => void;
}) {
  const enabled = providers.filter((p) => p.enabled);
  const [providerId, setProviderId] = useState(
    assignment?.provider_id ?? enabled[0]?.id ?? "",
  );
  const [modelId, setModelId] = useState(assignment?.model_id ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Follow the assignment when the listing changes (e.g. after a save).
  const provider = providers.find((p) => p.id === providerId);
  const models = provider?.models ?? [];
  const hasAssignment = assignment != null;

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!providerId) return;
    setBusy(true);
    setError(null);
    try {
      await setTeamAssignment(teamId, { provider_id: providerId, model_id: modelId });
      onSaved();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const clear = async () => {
    setBusy(true);
    setError(null);
    try {
      await clearTeamAssignment(teamId);
      setProviderId(enabled[0]?.id ?? "");
      setModelId("");
      onSaved();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={save} className="rounded-lg border border-border p-4">
      <div className="flex flex-wrap items-end gap-3">
        <div className="space-y-1.5">
          <Label htmlFor="assign-provider">{t("teamPage.labelProvider")}</Label>
          <NativeSelect
            id="assign-provider"
            value={providerId}
            onChange={(e) => {
              setProviderId(e.target.value);
              setModelId("");
            }}
            disabled={enabled.length === 0}
          >
            {enabled.length === 0 && (
              <NativeSelectOption value="">{t("teamPage.noEnabledProviders")}</NativeSelectOption>
            )}
            {enabled.map((p) => (
              <NativeSelectOption key={p.id} value={p.id}>
                {p.name}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="assign-model">{t("teamPage.labelModel")}</Label>
          <NativeSelect
            id="assign-model"
            value={modelId}
            onChange={(e) => setModelId(e.target.value)}
            disabled={models.length === 0}
          >
            <NativeSelectOption value="">
              {models.length > 0 ? t("teamPage.providerDefault") : t("teamPage.noModels")}
            </NativeSelectOption>
            {models
              .filter((m) => m.enabled)
              .map((m) => (
                <NativeSelectOption key={m.id} value={m.id}>
                  {m.display_name || m.name}
                </NativeSelectOption>
              ))}
          </NativeSelect>
        </div>
        <Button type="submit" disabled={busy || !providerId}>
          {busy ? t("teamPage.saving") : hasAssignment ? t("teamPage.change") : t("teamPage.assign")}
        </Button>
        {hasAssignment && (
          <Button
            type="button"
            variant="outline"
            onClick={clear}
            disabled={busy}
          >
            {t("teamPage.usePlatformDefault")}
          </Button>
        )}
      </div>
      {hasAssignment && (
        <p className="mt-2 text-xs text-muted-foreground">
          {t("teamPage.assignedTo")}{" "}
          <span className="font-medium">
            {providers.find((p) => p.id === assignment.provider_id)?.name ??
              t("teamPage.aProvider")}
          </span>
          . {t("teamPage.clearingFallsBack")}
        </p>
      )}
      {error && (
        <div className="mt-2">
          <ErrorNotice message={error} />
        </div>
      )}
    </form>
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
      <AsyncSection state={state} loadingLabel={t("teamPage.loadingUsage")}>
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
      <AsyncSection state={state} loadingLabel={t("teamPage.loadingMemories")}>
        {(data) => (
          <MemoryTable
            memories={data.memories}
            emptyMessage={t("teamPage.noSharedMemories")}
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
