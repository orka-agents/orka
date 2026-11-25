import { useState } from 'react'
import { Bell } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { formatTimestamp, timeAgo } from '@/lib/time'
import { cn } from '@/lib/utils'
import { useOversightAlerts } from '@/stores/oversight-alerts'
import { useUIStore } from '@/stores/ui'

/** The session's charter alerts, with an unread count. Read marks stay in this browser. */
export function AlertsButton({ onOpenDecision }: { onOpenDecision: (id: string) => void }) {
  const namespace = useUIStore((s) => s.namespace)
  const all = useOversightAlerts((s) => s.alerts)
  const markAllRead = useOversightAlerts((s) => s.markAllRead)
  const [open, setOpen] = useState(false)
  const alerts = all.filter((a) => a.namespace === namespace)
  const unread = alerts.filter((a) => !a.read).length
  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)} aria-label={`Alerts, ${unread} unread`}>
        <Bell aria-hidden="true" /> Alerts
        {unread > 0 && (
          <span className="rounded-full bg-status-failed-bg px-1.5 text-xs font-semibold tabular-nums text-status-failed" data-testid="alerts-unread">
            {unread}
          </span>
        )}
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>Charter alerts</DialogTitle>
            <DialogDescription>
              Actions that went against the charter while this page was open. Audit mode: each would pause; none was held.
            </DialogDescription>
          </DialogHeader>
          {alerts.length === 0 ? (
            <p className="text-sm text-muted-foreground">No charter alerts in this session.</p>
          ) : (
            <ul className="max-h-[60vh] divide-y overflow-y-auto" aria-label="Charter alerts">
              {alerts.map((alert) => (
                <li key={alert.decisionID} className={cn('flex items-start justify-between gap-3 py-2', alert.read && 'text-muted-foreground')}>
                  <div className="min-w-0 space-y-0.5 text-sm">
                    <p>
                      {!alert.read && <span className="sr-only">Unread: </span>}
                      <span className="font-medium">{alert.rule}</span>
                      {alert.charter && <span className="text-muted-foreground"> of {alert.charter}</span>}
                      {' '}broken by <span className="font-mono text-xs">{alert.agentName || alert.taskName}</span>, would pause (audit mode)
                    </p>
                    <p className="text-xs break-words text-muted-foreground">{alert.sentence}</p>
                    <p className="text-xs text-muted-foreground" title={formatTimestamp(alert.observedAt)}>{timeAgo(alert.observedAt)}</p>
                  </div>
                  <Button
                    size="xs"
                    variant="outline"
                    onClick={() => {
                      setOpen(false)
                      onOpenDecision(alert.decisionID)
                    }}
                    aria-label={`Inspect ${alert.decisionID}`}
                  >
                    Inspect
                  </Button>
                </li>
              ))}
            </ul>
          )}
          <div className="flex justify-end">
            <Button size="sm" variant="ghost" onClick={markAllRead} disabled={unread === 0}>Mark all read</Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}
