// The caller's teams: a list, plus creating one.

import { useState } from "react";
import { Link } from "react-router-dom";
import { Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { createTeam, myTeams } from "@/lib/admin";
import { useConsoleMe } from "@/components/admin/AdminLayout";
import {
  AsyncSection,
  ErrorNotice,
  formatDate,
  PageHeader,
  RoleBadge,
  useAsync,
} from "@/components/admin/common";

export function TeamsPage() {
  const { reload: reloadMe } = useConsoleMe();
  const state = useAsync(() => myTeams(), []);

  const afterCreate = () => {
    state.reload();
    // The sidebar renders from the profile, so it needs the new team too.
    reloadMe();
  };

  return (
    <>
      <PageHeader
        title="My teams"
        description="Teams share long-term memories, skills, and provider credentials with their members."
        actions={<CreateTeamDialog onCreated={afterCreate} />}
      />
      <AsyncSection state={state} loadingLabel="Loading teams">
        {(data) =>
          data.teams.length === 0 ? (
            <p className="rounded-lg border border-dashed border-border px-4 py-10 text-center text-sm text-muted-foreground">
              You do not belong to any team yet. Create one and you become its owner.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Team</TableHead>
                  <TableHead className="w-32">Your role</TableHead>
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
                    <TableCell>{t.role && <RoleBadge role={t.role} />}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {formatDate(t.created_at)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )
        }
      </AsyncSection>
    </>
  );
}

function CreateTeamDialog({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await createTeam(name.trim());
      setOpen(false);
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
            New team
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>Create a team</DialogTitle>
            <DialogDescription>
              You become its owner and can add members afterwards.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5 py-4">
            <Label htmlFor="team-name">Name</Label>
            <Input
              id="team-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Platform team"
              autoFocus
            />
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button type="submit" disabled={busy || name.trim() === ""}>
              {busy ? "Creating…" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
