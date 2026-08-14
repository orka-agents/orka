import { useState } from 'react'
import { Layers, Plus } from 'lucide-react'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import {
  useCreateSubstrateActorPool,
  useDeleteSubstrateActorPool,
  useSubstrateActorPools,
  useUpdateSubstrateActorPool,
} from '@/hooks/use-runtimes'
import { useUIStore } from '@/stores/ui'
import type { SubstrateActorPool, SubstrateActorPoolSpec } from '@/schemas/runtime'

interface PoolDraft {
  name: string
  editingName?: string
  templateRef: string
  workerPoolRef: string
  targetActors: string
  targetWorkers: string
  precreateActors: boolean
}

const emptyDraft: PoolDraft = {
  name: '',
  templateRef: '',
  workerPoolRef: '',
  targetActors: '',
  targetWorkers: '',
  precreateActors: false,
}

function phaseClass(phase?: string) {
  switch (phase) {
    case 'Ready':
      return 'bg-status-succeeded-bg text-status-succeeded'
    case 'Failed':
      return 'bg-status-failed-bg text-status-failed'
    default:
      return 'bg-status-pending-bg text-status-pending'
  }
}

function draftFromPool(pool: SubstrateActorPool): PoolDraft {
  return {
    name: pool.metadata.name,
    editingName: pool.metadata.name,
    templateRef: pool.spec.templateRef?.name ?? '',
    workerPoolRef: pool.spec.workerPoolRef?.name ?? '',
    targetActors: pool.spec.targetActors?.toString() ?? '',
    targetWorkers: pool.spec.targetWorkers?.toString() ?? '',
    precreateActors: pool.spec.precreateActors ?? false,
  }
}

export function SubstratePoolsPanel() {
  const namespace = useUIStore((s) => s.namespace)
  const { data, isLoading, error } = useSubstrateActorPools()
  const createPool = useCreateSubstrateActorPool()
  const updatePool = useUpdateSubstrateActorPool()
  const deletePool = useDeleteSubstrateActorPool()
  const [draft, setDraft] = useState<PoolDraft | null>(null)
  const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null)

  const items = data?.items ?? []

  const save = async () => {
    if (!draft) return
    if (!draft.editingName && !draft.name.trim()) {
      toast.error('Name is required')
      return
    }
    const spec: SubstrateActorPoolSpec = {
      ...(draft.templateRef.trim() ? { templateRef: { name: draft.templateRef.trim() } } : {}),
      ...(draft.workerPoolRef.trim() ? { workerPoolRef: { name: draft.workerPoolRef.trim() } } : {}),
      ...(draft.targetActors ? { targetActors: Number(draft.targetActors) } : {}),
      ...(draft.targetWorkers ? { targetWorkers: Number(draft.targetWorkers) } : {}),
      ...(draft.precreateActors ? { precreateActors: true } : {}),
    }
    try {
      if (draft.editingName) {
        await updatePool.mutateAsync({ name: draft.editingName, spec })
        toast.success(`Pool ${draft.editingName} updated`)
      } else {
        await createPool.mutateAsync({ name: draft.name.trim(), namespace, spec })
        toast.success(`Pool ${draft.name.trim()} created`)
      }
      setDraft(null)
    } catch (mutationError) {
      toast.error(`Failed to save pool: ${mutationError instanceof Error ? mutationError.message : 'unknown error'}`)
    }
  }

  const handleDelete = async (name: string) => {
    if (confirmingDelete !== name) {
      setConfirmingDelete(name)
      return
    }
    try {
      await deletePool.mutateAsync(name)
      toast.success(`Pool ${name} deleted`)
    } catch (mutationError) {
      toast.error(`Failed to delete pool: ${mutationError instanceof Error ? mutationError.message : 'unknown error'}`)
    } finally {
      setConfirmingDelete(null)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <Button onClick={() => setDraft(emptyDraft)}>
          <Plus className="mr-2 h-4 w-4" />
          New actor pool
        </Button>
      </div>
      {isLoading && <Skeleton className="h-40 w-full" />}
      {error != null && (
        <EmptyState
          icon={Layers}
          headline="Could not load substrate actor pools"
          hint={error instanceof Error ? error.message : 'The request failed.'}
        />
      )}
      {!isLoading && !error && items.length === 0 && (
        <EmptyState
          icon={Layers}
          headline="No substrate actor pools"
          hint="Actor pools pre-warm Substrate workers for MCP tools and future execution workspaces."
        />
      )}
      {items.map((pool) => (
        <Card key={pool.metadata.uid ?? pool.metadata.name}>
          <CardHeader>
            <CardTitle className="flex flex-wrap items-center justify-between gap-2">
              <span className="flex items-center gap-2">
                <Layers className="h-4 w-4 text-primary" />
                {pool.metadata.name}
              </span>
              <span className="flex items-center gap-2">
                <Badge variant="secondary" className={phaseClass(pool.status?.phase)}>
                  {pool.status?.phase ?? 'Unknown'}
                </Badge>
                <Button variant="ghost" size="sm" onClick={() => setDraft(draftFromPool(pool))}>Edit</Button>
                <Button
                  variant={confirmingDelete === pool.metadata.name ? 'destructive' : 'ghost'}
                  size="sm"
                  onClick={() => handleDelete(pool.metadata.name)}
                >
                  {confirmingDelete === pool.metadata.name ? 'Confirm delete' : 'Delete'}
                </Button>
              </span>
            </CardTitle>
          </CardHeader>
          <CardContent className="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
            <div><span className="text-muted-foreground">Template</span><div className="font-mono text-xs">{pool.spec.templateRef?.name ?? '—'}</div></div>
            <div><span className="text-muted-foreground">Worker pool</span><div className="font-mono text-xs">{pool.spec.workerPoolRef?.name ?? '—'}</div></div>
            <div>
              <span className="text-muted-foreground">Actors</span>
              <div className="font-mono">{pool.status?.actorCount ?? 0} / {pool.spec.targetActors ?? 0}
                <span className="ml-1 text-xs text-muted-foreground">({pool.status?.runningActorCount ?? 0} running, {pool.status?.suspendedActorCount ?? 0} suspended)</span>
              </div>
            </div>
            <div>
              <span className="text-muted-foreground">Workers</span>
              <div className="font-mono">{pool.status?.workerCount ?? 0} / {pool.spec.targetWorkers ?? 0}
                {pool.status?.actorsPerWorker && <span className="ml-1 text-xs text-muted-foreground">({pool.status.actorsPerWorker}/worker)</span>}
              </div>
            </div>
            {pool.status?.message && <p className="col-span-full text-sm text-muted-foreground">{pool.status.message}</p>}
          </CardContent>
        </Card>
      ))}

      <Dialog open={draft !== null} onOpenChange={(open) => { if (!open) setDraft(null) }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{draft?.editingName ? `Edit ${draft.editingName}` : 'New actor pool'}</DialogTitle>
            <DialogDescription>
              Substrate actor pools keep warm gVisor actors ready for MCP tool servers.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3 sm:grid-cols-2">
            {!draft?.editingName && (
              <div className="space-y-1.5 sm:col-span-2">
                <label htmlFor="pool-name" className="text-sm font-medium">Name</label>
                <Input id="pool-name" value={draft?.name ?? ''} onChange={(e) => setDraft((d) => (d ? { ...d, name: e.target.value } : d))} />
              </div>
            )}
            <div className="space-y-1.5">
              <label htmlFor="pool-template" className="text-sm font-medium">Actor template</label>
              <Input id="pool-template" value={draft?.templateRef ?? ''} onChange={(e) => setDraft((d) => (d ? { ...d, templateRef: e.target.value } : d))} />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="pool-worker" className="text-sm font-medium">Worker pool</label>
              <Input id="pool-worker" value={draft?.workerPoolRef ?? ''} onChange={(e) => setDraft((d) => (d ? { ...d, workerPoolRef: e.target.value } : d))} />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="pool-actors" className="text-sm font-medium">Target actors</label>
              <Input id="pool-actors" type="number" min="0" value={draft?.targetActors ?? ''} onChange={(e) => setDraft((d) => (d ? { ...d, targetActors: e.target.value } : d))} />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="pool-workers" className="text-sm font-medium">Target workers</label>
              <Input id="pool-workers" type="number" min="0" value={draft?.targetWorkers ?? ''} onChange={(e) => setDraft((d) => (d ? { ...d, targetWorkers: e.target.value } : d))} />
            </div>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <Switch
                checked={draft?.precreateActors ?? false}
                onCheckedChange={(checked) => setDraft((d) => (d ? { ...d, precreateActors: checked } : d))}
                aria-label="Precreate actors"
              />
              Precreate actors
            </label>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDraft(null)}>Cancel</Button>
            <Button onClick={save} disabled={createPool.isPending || updatePool.isPending}>
              {draft?.editingName ? 'Save changes' : 'Create pool'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
