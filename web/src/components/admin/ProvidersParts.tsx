// Shared building blocks for the provider registry UI (change provider-registry):
// a provider form, a model form, and a provider card with its models. Both the
// platform providers page (system CRUD) and the team providers tab (team-owned
// CRUD + assignment) use these, wiring the API calls themselves so the two
// tiers can target different routes.

import { useEffect, useState, type ReactElement } from "react";
import { Check, CloudDownload, Loader2, Pencil, Plus, Search, Star, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
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
import { ErrorNotice } from "@/components/admin/common";
import { ConfirmButton } from "@/components/admin/confirm";
import { cn } from "@/lib/utils";
import type {
  FetchedModel,
  Provider,
  ProviderBody,
  ProviderModel,
  ProviderModelBody,
} from "@/lib/admin";

export const VENDORS = ["anthropic", "openai"] as const;

// ---- provider form (create + edit) ----

export function ProviderFormDialog({
  open,
  onOpenChange,
  trigger,
  title,
  description,
  initial,
  submitLabel,
  onSave,
  onDone,
}: {
  // Controlled mode (edit flows): pass open + onOpenChange. Trigger mode
  // (create): pass trigger and omit open.
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  trigger?: ReactElement;
  title: string;
  description: string;
  initial?: Provider;
  submitLabel: string;
  onSave: (body: ProviderBody) => Promise<unknown>;
  onDone: () => void;
}) {
  const [selfOpen, setSelfOpen] = useState(false);
  const [name, setName] = useState(initial?.name ?? "");
  const [vendor, setVendor] = useState<string>(initial?.vendor ?? VENDORS[0]);
  const [baseUrl, setBaseUrl] = useState(initial?.base_url ?? "");
  const [apiKey, setApiKey] = useState("");
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const isOpen = open ?? selfOpen;
  const setOpen = (v: boolean) => {
    if (onOpenChange) onOpenChange(v);
    else setSelfOpen(v);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onSave({
        name: name.trim(),
        vendor: vendor as ProviderBody["vendor"],
        base_url: baseUrl.trim(),
        // Only send api_key when the user typed one; an empty/omitted key on
        // edit leaves the stored key untouched, on create it stores none.
        ...(apiKey ? { api_key: apiKey.trim() } : {}),
        enabled,
      });
      setOpen(false);
      setApiKey("");
      onDone();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={isOpen} onOpenChange={setOpen}>
      {trigger && <DialogTrigger render={trigger} />}
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="prov-name">Name</Label>
              <Input
                id="prov-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. Primary Anthropic"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="prov-vendor">Vendor</Label>
              <NativeSelect
                id="prov-vendor"
                value={vendor}
                onChange={(e) => setVendor(e.target.value)}
              >
                {VENDORS.map((v) => (
                  <NativeSelectOption key={v} value={v}>
                    {v}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="prov-base">Base URL</Label>
              <Input
                id="prov-base"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
                placeholder="https://api.openai.com/v1"
              />
              <p className="text-xs text-muted-foreground">
                The API root ending in <code>/v1</code> (or the vendor default
                when left empty) — the chat and model-list paths are appended
                automatically. A legacy full endpoint is accepted too.
              </p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="prov-key">API key</Label>
              <Input
                id="prov-key"
                type="password"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder={
                  initial ? "Leave empty to keep the stored key" : "sk-…"
                }
                autoComplete="off"
              />
              {initial && (
                <p className="text-xs text-muted-foreground">
                  Keys are write-only — the stored key shows only its last four
                  characters.
                </p>
              )}
            </div>
            <div className="flex items-center justify-between">
              <Label htmlFor="prov-enabled">Enabled</Label>
              <Switch
                id="prov-enabled"
                checked={enabled}
                onCheckedChange={setEnabled}
              />
            </div>
          </div>
          {error && <ErrorNotice message={error} />}
          <DialogFooter>
            <Button
              type="submit"
              disabled={busy || name.trim() === ""}
            >
              {busy ? "Saving…" : submitLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- model form (create + edit) ----

export function ModelFormDialog({
  open,
  onOpenChange,
  trigger,
  title,
  description,
  initial,
  onSave,
  onDone,
}: {
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  trigger?: ReactElement;
  title: string;
  description: string;
  initial?: ProviderModel;
  onSave: (body: ProviderModelBody) => Promise<unknown>;
  onDone: () => void;
}) {
  const [selfOpen, setSelfOpen] = useState(false);
  const [name, setName] = useState(initial?.name ?? "");
  const [displayName, setDisplayName] = useState(initial?.display_name ?? "");
  const [vision, setVision] = useState(initial?.vision ?? false);
  const [windowText, setWindowText] = useState(
    initial?.context_window != null ? String(initial.context_window) : "",
  );
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const isOpen = open ?? selfOpen;
  const setOpen = (v: boolean) => {
    if (onOpenChange) onOpenChange(v);
    else setSelfOpen(v);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onSave({
        name: name.trim(),
        ...(displayName ? { display_name: displayName.trim() } : {}),
        vision,
        context_window: windowText.trim() === "" ? null : Number(windowText),
        enabled,
      });
      setOpen(false);
      onDone();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={isOpen} onOpenChange={setOpen}>
      {trigger && <DialogTrigger render={trigger} />}
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="model-name">Model ID</Label>
              <Input
                id="model-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. gpt-4o-mini"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="model-display">Display name</Label>
              <Input
                id="model-display"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                placeholder="Optional, shown instead of the model ID"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="model-window">Context window</Label>
              <Input
                id="model-window"
                type="number"
                min={0}
                value={windowText}
                onChange={(e) => setWindowText(e.target.value)}
                placeholder="Empty = derive from the capability table"
              />
            </div>
            <div className="flex items-center justify-between">
              <Label htmlFor="model-vision">Vision-capable</Label>
              <Switch id="model-vision" checked={vision} onCheckedChange={setVision} />
            </div>
            <div className="flex items-center justify-between">
              <Label htmlFor="model-enabled">Enabled</Label>
              <Switch id="model-enabled" checked={enabled} onCheckedChange={setEnabled} />
            </div>
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

// ---- fetch-models dialog (preview + selection, never auto-registers) ----

export function FetchModelsDialog({
  open,
  onOpenChange,
  fetchModels,
  addModel,
  onDone,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  fetchModels: () => Promise<FetchedModel[]>;
  addModel: (name: string) => Promise<unknown>;
  onDone: () => void;
}) {
  const [models, setModels] = useState<FetchedModel[] | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [filter, setFilter] = useState("");
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    setModels(null);
    setSelected(new Set());
    setFilter("");
    fetchModels()
      .then((ms) => {
        if (cancelled) return;
        setModels(ms);
        // Default-select the not-yet-registered models.
        setSelected(new Set(ms.filter((m) => !m.registered).map((m) => m.name)));
      })
      .catch((err) => {
        if (!cancelled) setError((err as Error).message);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, fetchModels]);

  const toggle = (name: string, checked: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (checked) next.add(name);
      else next.delete(name);
      return next;
    });
  };

  const selectAll = () => {
    setSelected(new Set(unregisteredVisible.map((m) => m.name)));
  };

  const selectNone = () => {
    setSelected(new Set());
  };

  const add = async () => {
    setBusy(true);
    setError(null);
    try {
      for (const name of selected) {
        await addModel(name);
      }
      onOpenChange(false);
      onDone();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // visible narrows the list to the search box; it drives rendering, the
  // select-all scope, and the "n of m" count so actions match what is shown.
  const query = filter.trim().toLowerCase();
  const visible =
    query === "" || !models
      ? models ?? []
      : models.filter((m) => m.name.toLowerCase().includes(query));
  const unregisteredVisible = visible.filter((m) => !m.registered);
  const selectedCount = [...selected].length;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Fetch models</DialogTitle>
          <DialogDescription>
            The models this provider's API reports. Fetching is only a preview —
            pick the ones to add; already-registered models are disabled.
          </DialogDescription>
        </DialogHeader>
        {!loading && !error && models && models.length > 0 && (
          <div className="relative">
            <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Search models…"
              className="pl-8"
              disabled={busy}
            />
          </div>
        )}
        {!loading && !error && unregisteredVisible.length > 0 && (
          <div className="flex items-center justify-between border-b border-border pb-1.5">
            <span className="text-xs text-muted-foreground">
              {selectedCount} of {unregisteredVisible.length} new model
              {unregisteredVisible.length === 1 ? "" : "s"} selected.
            </span>
            <div className="flex items-center gap-1">
              <Button
                variant="ghost"
                size="sm"
                onClick={selectAll}
                disabled={busy || selectedCount === unregisteredVisible.length}
              >
                Select all
              </Button>
              <Button
                variant="ghost"
                size="sm"
                onClick={selectNone}
                disabled={busy || selectedCount === 0}
              >
                Clear
              </Button>
            </div>
          </div>
        )}
        <div className="max-h-72 space-y-1 overflow-y-auto py-2">
          {loading && (
            <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              Fetching from the provider…
            </div>
          )}
          {!loading && error && <ErrorNotice message={error} />}
          {!loading && !error && models && models.length === 0 && (
            <p className="py-4 text-sm text-muted-foreground">
              The provider reported no models.
            </p>
          )}
          {!loading && !error && query !== "" && visible.length === 0 && (
            <p className="py-4 text-sm text-muted-foreground">
              No models match "{filter}".
            </p>
          )}
          {!loading &&
            !error &&
            visible.map((m) => (
              <label
                key={m.name}
                className={cn(
                  "flex cursor-pointer items-center gap-2 rounded px-1 py-1 text-sm",
                  m.registered ? "cursor-default opacity-60" : "hover:bg-muted/60",
                )}
              >
                <Checkbox
                  checked={selected.has(m.name)}
                  disabled={m.registered}
                  onCheckedChange={(v) => toggle(m.name, v === true)}
                />
                <span className="truncate font-mono text-xs">{m.name}</span>
                {m.registered && (
                  <Badge variant="secondary" className="ml-auto shrink-0">
                    added
                  </Badge>
                )}
              </label>
            ))}
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={busy}
          >
            Cancel
          </Button>
          <Button onClick={add} disabled={busy || selectedCount === 0}>
            {busy ? "Adding…" : `Add selected (${selectedCount})`}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---- provider card: one provider with its models ----

export function ProviderCard({
  provider,
  canWrite,
  assignment,
  onEdit,
  onDelete,
  onSetDefault,
  onFetchModels,
  onAddModel,
  onUpdateModel,
  onDeleteModel,
  onSetDefaultModel,
}: {
  provider: Provider;
  canWrite: boolean;
  // assignment, when non-nil, marks this provider as the team's selection.
  assignment?: boolean;
  onEdit?: (p: Provider) => void;
  onDelete?: (p: Provider) => void;
  onSetDefault?: (p: Provider) => void;
  // onFetchModels opens the fetch-models dialog: pull the provider's own model
  // list and let the user pick which to register.
  onFetchModels?: (p: Provider) => void;
  onAddModel?: (p: Provider) => void;
  onUpdateModel?: (p: Provider, m: ProviderModel) => void;
  onDeleteModel?: (p: Provider, m: ProviderModel) => void;
  onSetDefaultModel?: (p: Provider, m: ProviderModel) => void;
}) {
  return (
    <div className="rounded-lg border border-border">
      <div className="flex items-center gap-2 border-b border-border px-4 py-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="font-medium">{provider.name}</span>
            <Badge variant="outline" className="capitalize">
              {provider.vendor}
            </Badge>
            {provider.scope === "team" && (
              <Badge variant="secondary">team</Badge>
            )}
            {provider.is_default && (
              <Badge variant="secondary">
                <Star className="mr-1 size-3" />
                platform default
              </Badge>
            )}
            {assignment && (
              <Badge variant="secondary">
                <Check className="mr-1 size-3" />
                assigned
              </Badge>
            )}
            {!provider.enabled && <Badge variant="destructive">disabled</Badge>}
          </div>
          {provider.base_url && (
            <div className="truncate text-xs text-muted-foreground">
              {provider.base_url}
            </div>
          )}
        </div>
        <div className="flex items-center gap-1">
          {canWrite && onFetchModels && (
            <Button
              variant="ghost"
              size="sm"
              title="Fetch this provider's model list and add the ones you pick"
              onClick={() => onFetchModels(provider)}
            >
              <CloudDownload />
            </Button>
          )}
          {canWrite && onSetDefault && !provider.is_default && (
            <Button
              variant="ghost"
              size="sm"
              title="Make the platform default"
              onClick={() => onSetDefault(provider)}
            >
              <Star />
            </Button>
          )}
          {canWrite && onEdit && (
            <Button
              variant="ghost"
              size="sm"
              title="Edit provider"
              onClick={() => onEdit(provider)}
            >
              <Pencil />
            </Button>
          )}
          {canWrite && onDelete && (
            <ConfirmButton
              title={`Delete ${provider.name}?`}
              description="Its models go with it. A provider a team assigns to cannot be deleted until that assignment is cleared."
              confirmLabel="Delete"
              onConfirm={() => onDelete(provider)}
              trigger={
                <Button variant="ghost" size="sm" aria-label="Delete provider">
                  <Trash2 />
                </Button>
              }
            />
          )}
        </div>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Model</TableHead>
            <TableHead className="w-28">Context</TableHead>
            <TableHead className="w-24">Vision</TableHead>
            <TableHead className="w-28">Default</TableHead>
            <TableHead className="w-24">Enabled</TableHead>
            <TableHead className="w-24" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {(provider.models ?? []).map((m) => (
            <TableRow key={m.id}>
              <TableCell>
                <div className="font-medium">
                  {m.display_name || m.name}
                  {m.display_name && m.display_name !== m.name && (
                    <div className="text-xs font-normal text-muted-foreground">
                      {m.name}
                    </div>
                  )}
                </div>
              </TableCell>
              <TableCell>
                {m.context_window != null ? (
                  <span className="font-mono text-xs" title="Context window (tokens)">
                    {m.context_window.toLocaleString()}
                  </span>
                ) : (
                  <span className="text-muted-foreground" title="Derived from the capability table">
                    auto
                  </span>
                )}
              </TableCell>
              <TableCell>
                {m.vision ? <Badge variant="secondary">vision</Badge> : <span className="text-muted-foreground">—</span>}
              </TableCell>
              <TableCell>
                {m.is_default ? (
                  <Badge variant="secondary">default</Badge>
                ) : canWrite && onSetDefaultModel ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    title="Make this the provider's default model"
                    onClick={() => onSetDefaultModel(provider, m)}
                  >
                    <Star />
                  </Button>
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              <TableCell>
                {m.enabled ? (
                  <Badge variant="outline">on</Badge>
                ) : (
                  <Badge variant="destructive">off</Badge>
                )}
              </TableCell>
              <TableCell className="text-right">
                {canWrite && (
                  <div className="flex justify-end gap-1">
                    {onUpdateModel && (
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label="Edit model"
                        title="Edit model"
                        onClick={() => onUpdateModel(provider, m)}
                      >
                        <Pencil />
                      </Button>
                    )}
                    {onDeleteModel && (
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label="Delete model"
                        onClick={() => onDeleteModel(provider, m)}
                      >
                        <Trash2 />
                      </Button>
                    )}
                  </div>
                )}
              </TableCell>
            </TableRow>
          ))}
          {!provider.models || provider.models.length === 0 ? (
            <TableRow>
              <TableCell
                colSpan={6}
                className="py-4 text-center text-sm text-muted-foreground"
              >
                No models yet — add one so the provider can serve a run.
              </TableCell>
            </TableRow>
          ) : null}
        </TableBody>
      </Table>

      {canWrite && onAddModel && (
        <div className="border-t border-border px-4 py-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => onAddModel(provider)}
          >
            <Plus />
            Add model
          </Button>
        </div>
      )}
    </div>
  );
}


