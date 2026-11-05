import { MonitorPlay } from 'lucide-react'
import { useTaskLiveView } from '@/hooks/use-task-live-view'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'

// A view-only picture of what a running browser Task's page shows now. It
// renders nothing unless the operator turned live view on and this Task runs
// the browser image; frames are never stored, so it ends with the Task.
export function TaskLiveView({ taskId, uid, running, paused = false }: { taskId: string; uid?: string; running: boolean; paused?: boolean }) {
  const view = useTaskLiveView(taskId, running, paused, uid)
  if (view.status === 'off') return null

  return (
    <Card aria-label="Live view">
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <CardTitle className="flex items-center gap-2 text-base">
          <MonitorPlay className="h-4 w-4" aria-hidden="true" />
          Live view
        </CardTitle>
        <Badge variant="outline">View only</Badge>
      </CardHeader>
      <CardContent>
        {view.status === 'live' ? (
          <figure className="space-y-2">
            <img
              src={view.src}
              alt="What the Task's browser shows now"
              className="w-full rounded-md border bg-muted"
            />
            <figcaption className="text-xs text-muted-foreground">
              {view.frameTime ? `Page last changed at ${new Date(view.frameTime).toLocaleTimeString()}. ` : ''}
              {paused ? 'Paused: turn Following back on to update. ' : ''}
              Not recorded: this view ends with the Task.
            </figcaption>
          </figure>
        ) : (
          <p className="text-sm text-muted-foreground">Waiting for the browser to show a page.</p>
        )}
      </CardContent>
    </Card>
  )
}
