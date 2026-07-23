import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { GitPullRequest } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/lib/api-client'
import { useUIStore } from '@/stores/ui'

interface PRCreateDialogProps {
  taskName: string
  pushBranch?: string
  summary?: string
  targetBranch?: string
}

export function PRCreateDialog({ taskName, pushBranch, summary, targetBranch = 'main' }: PRCreateDialogProps) {
  const [open, setOpen] = useState(false)
  const [title, setTitle] = useState(`Changes from task ${taskName}`)
  const [body, setBody] = useState(summary ?? '')
  const [base, setBase] = useState(targetBranch)
  const [draft, setDraft] = useState(false)
  const [creating, setCreating] = useState(false)
  const namespace = useUIStore((s) => s.namespace)

  if (!pushBranch) return null

  const handleCreate = async () => {
    try {
      setCreating(true)
      await api.post(`/tasks/${taskName}/pr`, {
        title,
        body,
        head: pushBranch,
        base,
        draft,
        namespace,
      })
      toast.success('Pull request created')
      setOpen(false)
    } catch (err) {
      toast.error(`Failed to create PR: ${err instanceof Error ? err.message : 'Unknown error'}`)
    } finally {
      setCreating(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          <GitPullRequest className="mr-2 h-4 w-4" />
          Create PR
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Create Pull Request</DialogTitle>
          <DialogDescription className="sr-only">
            Create a pull request from the task branch to the selected base branch.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-2">
            <label htmlFor="pr-title" className="text-sm font-medium">Title</label>
            <Input id="pr-title" value={title} onChange={e => setTitle(e.target.value)} />
          </div>
          <div className="space-y-2">
            <label htmlFor="pr-description" className="text-sm font-medium">Description</label>
            <textarea
              id="pr-description"
              className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
              value={body}
              onChange={e => setBody(e.target.value)}
            />
          </div>
          <div className="grid gap-4 grid-cols-2">
            <div className="space-y-2">
              <label htmlFor="pr-head-branch" className="text-sm font-medium">Head Branch</label>
              <Input id="pr-head-branch" value={pushBranch} disabled />
            </div>
            <div className="space-y-2">
              <label htmlFor="pr-base-branch" className="text-sm font-medium">Base Branch</label>
              <Input id="pr-base-branch" value={base} onChange={e => setBase(e.target.value)} />
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Switch id="pr-draft" checked={draft} onCheckedChange={setDraft} />
            <label htmlFor="pr-draft" className="text-sm">Create as draft</label>
          </div>
          <div className="flex gap-2 justify-end">
            <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
            <Button onClick={handleCreate} disabled={creating || !title}>
              {creating ? 'Creating...' : 'Create Pull Request'}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
