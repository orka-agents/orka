import { useState } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState } from '@/components/ui/empty-state'
import { PageHeader } from '@/components/layout/page-header'
import { Activity, ArrowLeft, SearchX, ShieldCheck, Trash2, type LucideIcon } from 'lucide-react'
import { TranscriptViewer } from './transcript-viewer'
import { SessionEventTimeline } from './session-event-timeline'
import { SessionExecutionRouteLedger } from '@/components/execution/execution-route-ledger'
import { useSession, useDeleteSession } from '@/hooks/use-sessions'
import { isForbiddenError, isNotFoundError } from '@/lib/api-client'
import { toast } from 'sonner'

const SESSION_TABS = new Set(['overview', 'timeline'])

// A missing or unreadable session keeps the page heading (and so the tab
// title) and a way back to the list.
function SessionUnavailable({ title, icon, headline, hint, alert = false }: {
  title: string
  icon: LucideIcon
  headline: string
  hint: string
  alert?: boolean
}) {
  return (
    <div className="space-y-4">
      <PageHeader title={title} />
      <Card>
        <CardContent>
          <div role={alert ? 'alert' : undefined}>
            <EmptyState
              icon={icon}
              headline={headline}
              hint={hint}
              action={
                <Button asChild variant="outline" size="sm">
                  <Link to="/sessions"><ArrowLeft className="h-4 w-4" /> Back to sessions</Link>
                </Button>
              }
            />
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

export function SessionDetail({ sessionId }: { sessionId: string }) {
  const { data: session, isLoading, error } = useSession(sessionId)
  const deleteSession = useDeleteSession()
  const navigate = useNavigate()
  const search = useSearch({ from: '/sessions/$sessionId' })
  // Like the task detail: a local override answers a tab click instantly and
  // is dropped whenever the URL's tab changes, so the URL stays the source of
  // truth (deep links, Back from a linked task).
  const [tabState, setTabState] = useState<{ override: string | null; seen?: string }>({ override: null })
  if (tabState.seen !== search.tab) setTabState({ override: null, seen: search.tab })
  const requestedTab = tabState.override ?? search.tab ?? 'overview'
  const activeTab = SESSION_TABS.has(requestedTab) ? requestedTab : 'overview'
  const setTab = (tab: string) => {
    setTabState({ override: tab, seen: search.tab })
    navigate({ to: '/sessions/$sessionId', params: { sessionId }, search: { tab }, replace: true })
  }

  const handleDelete = async (name: string) => {
    if (deleteSession.isPending || !confirm(`Delete session "${name}"?`)) return
    try {
      await deleteSession.mutateAsync(name)
      toast.success('Session deleted')
      navigate({ to: '/sessions' })
    } catch (err) {
      toast.error(`Failed to delete session: ${err instanceof Error ? err.message : 'Unknown error'}`)
    }
  }

  if (isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  // A 403 or 404 wins over cached data (React Query keeps the last
  // successful `data` when a refetch fails); any other failure only replaces
  // the page when nothing was loaded. The timeline's own queries render only
  // with a loaded session.
  const missing = isNotFoundError(error)
  const forbidden = isForbiddenError(error)
  if (forbidden || (error && !missing && !session)) {
    const message = error instanceof Error ? error.message : ''
    return (
      <SessionUnavailable
        alert
        title="Could not load session"
        icon={forbidden ? ShieldCheck : Activity}
        headline={forbidden ? 'Not authorized to view sessions' : `Session "${sessionId}" failed to load`}
        hint={forbidden
          ? `Your token lacks sessions read permission${message ? ` (${message})` : ''}.`
          : message || 'The session request failed.'}
      />
    )
  }

  if (missing || !session) {
    return (
      <SessionUnavailable
        title="Session not found"
        icon={SearchX}
        headline={`No session named "${sessionId}"`}
        hint="It may have been deleted, or it belongs to another namespace."
      />
    )
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-4">
        <Button asChild variant="ghost" size="icon">
          <Link to="/sessions" aria-label="Back to sessions"><ArrowLeft className="h-4 w-4" /></Link>
        </Button>
        <PageHeader
          className="flex-1"
          title={session.name}
          description={session.namespace}
          action={
            <Button
              variant="destructive"
              size="sm"
              onClick={() => handleDelete(session.name)}
              // aria-disabled, not disabled: disabling the focused button would drop
              // keyboard focus to <body> when the delete fails.
              aria-disabled={deleteSession.isPending || undefined}
              className="aria-disabled:opacity-50"
            >
              <Trash2 className="mr-2 h-4 w-4" /> {deleteSession.isPending ? 'Deleting...' : 'Delete'}
            </Button>
          }
        />
      </div>

      <SessionExecutionRouteLedger session={session} />

      <Tabs value={activeTab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="timeline">Timeline</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="space-y-4">
          <div className="grid gap-4 md:grid-cols-4">
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm">Messages</CardTitle></CardHeader>
              <CardContent><p className="text-2xl font-bold">{session.messageCount ?? '0'}</p></CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm">Input Tokens</CardTitle></CardHeader>
              <CardContent><p className="text-2xl font-bold">{session.inputTokens ?? '0'}</p></CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm">Output Tokens</CardTitle></CardHeader>
              <CardContent><p className="text-2xl font-bold">{session.outputTokens ?? '0'}</p></CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2"><CardTitle className="text-sm">Active Task</CardTitle></CardHeader>
              <CardContent><p className="text-2xl font-bold">{session.activeTask || '-'}</p></CardContent>
            </Card>
          </div>

          <Card>
            <CardHeader><CardTitle>Transcript</CardTitle></CardHeader>
            <CardContent>
              <TranscriptViewer transcript={session.transcript} />
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="timeline">
          <SessionEventTimeline key={`${session.namespace}/${session.name}`} sessionId={session.name} />
        </TabsContent>
      </Tabs>
    </div>
  )
}
