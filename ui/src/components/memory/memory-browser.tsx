import { useState } from 'react'
import { Brain, Plus, Search } from 'lucide-react'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import { EmptyState } from '@/components/ui/empty-state'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import {
  isMemoryStoreUnavailable,
  useCreateMemory,
  useDeleteMemory,
  useMemoryList,
  useSetMemoryEnabled,
  useUpdateMemory,
} from '@/hooks/use-memory'
import type { Memory } from '@/schemas/memory'

interface MemoryDraft {
  id?: string
  content: string
  tags: string
}

const emptyDraft: MemoryDraft = { content: '', tags: '' }

export function MemoryBrowser() {
  const [query, setQuery] = useState('')
  const [source, setSource] = useState('')
  const [includeDisabled, setIncludeDisabled] = useState(false)
  const [includeDeleted, setIncludeDeleted] = useState(false)
  const [draft, setDraft] = useState<MemoryDraft | null>(null)

  const { data, isLoading, error } = useMemoryList({
    query: query || undefined,
    source: source || undefined,
    includeDisabled,
    includeDeleted,
  })
  const createMemory = useCreateMemory()
  const updateMemory = useUpdateMemory()
  const toggleMemory = useSetMemoryEnabled()
  const deleteMemory = useDeleteMemory()

  if (error && isMemoryStoreUnavailable(error)) {
    return (
      <EmptyState
        icon={Brain}
        headline="Memory store is not enabled"
        hint="The controller has no memory store configured. Durable memory, recall, and proposals activate once one is set up."
      />
    )
  }

  const items = data?.items ?? []

  const saveDraft = async () => {
    if (!draft) return
    if (!draft.content.trim()) {
      toast.error('Memory content is required')
      return
    }
    const tags = draft.tags.split(',').map((t) => t.trim()).filter(Boolean)
    try {
      if (draft.id) {
        await updateMemory.mutateAsync({ id: draft.id, content: draft.content, tags })
        toast.success('Memory updated')
      } else {
        await createMemory.mutateAsync({ content: draft.content, tags, source: 'manual' })
        toast.success('Memory created')
      }
      setDraft(null)
    } catch (mutationError) {
      toast.error(`Failed to save memory: ${mutationError instanceof Error ? mutationError.message : 'unknown error'}`)
    }
  }

  const handleToggle = async (memory: Memory) => {
    try {
      await toggleMemory.mutateAsync({ id: memory.id, enabled: Boolean(memory.disabled) })
    } catch (mutationError) {
      toast.error(`Failed to update memory: ${mutationError instanceof Error ? mutationError.message : 'unknown error'}`)
    }
  }

  const handleDelete = async (memory: Memory) => {
    try {
      await deleteMemory.mutateAsync(memory.id)
      toast.success('Memory deleted (soft)')
    } catch (mutationError) {
      toast.error(`Failed to delete memory: ${mutationError instanceof Error ? mutationError.message : 'unknown error'}`)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end gap-3">
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search content…"
            aria-label="Search memories"
            className="w-64 pl-8"
          />
        </div>
        <Input
          value={source}
          onChange={(e) => setSource(e.target.value)}
          placeholder="Filter by source"
          aria-label="Filter by source"
          className="w-44"
        />
        <label className="flex items-center gap-2 text-sm">
          <Switch checked={includeDisabled} onCheckedChange={setIncludeDisabled} aria-label="Include disabled" />
          Disabled
        </label>
        <label className="flex items-center gap-2 text-sm">
          <Switch checked={includeDeleted} onCheckedChange={setIncludeDeleted} aria-label="Include deleted" />
          Deleted
        </label>
        <Button className="ml-auto" onClick={() => setDraft(emptyDraft)}>
          <Plus className="mr-2 h-4 w-4" />
          New memory
        </Button>
      </div>

      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Content</TableHead>
              <TableHead>Tags</TableHead>
              <TableHead>Source</TableHead>
              <TableHead>Recalls</TableHead>
              <TableHead>State</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: 3 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 6 }).map((_, j) => (
                    <TableCell key={j}><Skeleton className="h-4 w-20" /></TableCell>
                  ))}
                </TableRow>
              ))
            ) : items.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} className="py-8 text-center text-muted-foreground">
                  No memories match. Agents add durable memory only through applied proposals; you can also add one directly.
                </TableCell>
              </TableRow>
            ) : (
              items.map((memory) => (
                <TableRow key={memory.id} className={memory.deleted ? 'opacity-60' : undefined}>
                  <TableCell className="max-w-md">
                    <p className="truncate" title={memory.content}>{memory.content}</p>
                    <p className="text-xs text-muted-foreground">
                      {memory.agentName || memory.taskName
                        ? `from ${memory.agentName ?? memory.taskName}`
                        : memory.sourceProposalId
                          ? 'from applied proposal'
                          : ''}
                    </p>
                  </TableCell>
                  <TableCell>
                    <div className="flex max-w-40 flex-wrap gap-1">
                      {(memory.tags ?? []).map((tag) => (
                        <Badge key={tag} variant="secondary">{tag}</Badge>
                      ))}
                    </div>
                  </TableCell>
                  <TableCell>{memory.source || '-'}</TableCell>
                  <TableCell>{memory.recalledCount ?? 0}</TableCell>
                  <TableCell>
                    {memory.deleted ? (
                      <Badge variant="secondary" className="bg-status-failed-bg text-status-failed">Deleted</Badge>
                    ) : memory.disabled ? (
                      <Badge variant="secondary" className="bg-status-pending-bg text-status-pending">Disabled</Badge>
                    ) : (
                      <Badge variant="secondary" className="bg-status-succeeded-bg text-status-succeeded">Active</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      {!memory.deleted && (
                        <>
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => handleToggle(memory)}
                          >
                            {memory.disabled ? 'Enable' : 'Disable'}
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => setDraft({ id: memory.id, content: memory.content, tags: (memory.tags ?? []).join(', ') })}
                          >
                            Edit
                          </Button>
                          <Button variant="ghost" size="sm" onClick={() => handleDelete(memory)}>
                            Delete
                          </Button>
                        </>
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>

      <Dialog open={draft !== null} onOpenChange={(open) => { if (!open) setDraft(null) }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{draft?.id ? 'Edit memory' : 'New memory'}</DialogTitle>
            <DialogDescription>
              Durable memory is injected into agent context on recall. Never store secrets, tokens, or transcripts.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <textarea
              value={draft?.content ?? ''}
              onChange={(e) => setDraft((d) => (d ? { ...d, content: e.target.value } : d))}
              rows={6}
              aria-label="Memory content"
              className="w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring"
              placeholder="The deploy pipeline requires make manifests before make build…"
            />
            <Input
              value={draft?.tags ?? ''}
              onChange={(e) => setDraft((d) => (d ? { ...d, tags: e.target.value } : d))}
              aria-label="Tags"
              placeholder="Tags (comma-separated)"
            />
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDraft(null)}>Cancel</Button>
            <Button onClick={saveDraft} disabled={createMemory.isPending || updateMemory.isPending}>
              {draft?.id ? 'Save changes' : 'Create memory'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
