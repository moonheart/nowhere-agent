// Platform provider registry (change provider-registry): system providers and
// their models, managed by a platform administrator. One system provider is the
// platform default every team without an assignment falls back to.

import { useState } from "react";
import { Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  createSystemModel,
  createSystemProvider,
  deleteSystemModel,
  deleteSystemProvider,
  fetchSystemModels,
  listSystemProviders,
  setSystemDefaultModel,
  setSystemDefaultProvider,
  updateSystemModel,
  updateSystemProvider,
  type Provider,
  type ProviderModel,
} from "@/lib/admin";
import {
  AsyncSection,
  ErrorNotice,
  PageHeader,
  useAsync,
} from "@/components/admin/common";
import {
  FetchModelsDialog,
  ModelFormDialog,
  ProviderCard,
  ProviderFormDialog,
} from "@/components/admin/ProvidersParts";

export function PlatformProvidersPage() {
  const state = useAsync(() => listSystemProviders(), []);
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
    <>
      <PageHeader
        title="Providers"
        description="System LLM providers the platform makes available. Every team without an assignment of its own falls back to the platform default."
        actions={
          <ProviderFormDialog
            trigger={
              <Button size="sm">
                <Plus />
                Add provider
              </Button>
            }
            title="Add a provider"
            description="System providers are visible to every team. Keys are encrypted at rest when a master key is configured."
            submitLabel="Add provider"
            onSave={(b) => createSystemProvider(b)}
            onDone={state.reload}
          />
        }
      />
      {error && <ErrorNotice message={error} />}
      <AsyncSection state={state} loadingLabel="Loading providers">
        {(data) =>
          data.providers.length === 0 ? (
            <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
              No system providers configured. Add one to enable chat and
              scheduled tasks, then set a default model and mark it the platform
              default.
            </p>
          ) : (
            <div className="space-y-4">
              {data.providers.map((p) => (
                <ProviderCard
                  key={p.id}
                  provider={p}
                  canWrite
                  onEdit={() => setEditing(p)}
                  onDelete={(pr) => act(() => deleteSystemProvider(pr.id))}
                  onSetDefault={(pr) => act(() => setSystemDefaultProvider(pr.id))}
                  onFetchModels={() => setFetching(p)}
                  onAddModel={() => setAddingModelTo(p)}
                  onUpdateModel={(pr, m) => setEditingModel({ provider: pr, model: m })}
                  onDeleteModel={(pr, m) => act(() => deleteSystemModel(pr.id, m.id))}
                  onSetDefaultModel={(pr, m) =>
                    act(() => setSystemDefaultModel(pr.id, m.id))
                  }
                />
              ))}
            </div>
          )
        }
      </AsyncSection>

      {editing && (
        <ProviderFormDialog
          open
          onOpenChange={(open) => !open && setEditing(null)}
          title="Edit provider"
          description="Changes apply to the next model call."
          initial={editing}
          submitLabel="Save"
          onSave={(b) => updateSystemProvider(editing.id, b)}
          onDone={state.reload}
        />
      )}
      {fetching && (
        <FetchModelsDialog
          open
          onOpenChange={(open) => !open && setFetching(null)}
          fetchModels={() => fetchSystemModels(fetching.id).then((r) => r.models)}
          addModel={(name) => createSystemModel(fetching.id, { name })}
          onDone={state.reload}
        />
      )}
      {addingModelTo && (
        <ModelFormDialog
          open
          onOpenChange={(open) => !open && setAddingModelTo(null)}
          title={`Add a model to ${addingModelTo.name}`}
          description="A vision-capable model backs the view_image tool for providers whose main model cannot see images."
          onSave={(b) => createSystemModel(addingModelTo.id, b)}
          onDone={state.reload}
        />
      )}
      {editingModel && (
        <ModelFormDialog
          open
          onOpenChange={(open) => !open && setEditingModel(null)}
          title="Edit model"
          description="Changes apply to the next model call."
          initial={editingModel.model}
          onSave={(b) =>
            updateSystemModel(editingModel.provider.id, editingModel.model.id, b)
          }
          onDone={state.reload}
        />
      )}
    </>
  );
}
