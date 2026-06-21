import { Link, useNavigate } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { ArrowLeft, Trash2 } from 'lucide-react'
import { TranscriptViewer } from './transcript-viewer'
import { SessionEventTimeline } from './session-event-timeline'
import { useSession, useDeleteSession } from '@/hooks/use-sessions'

export function SessionDetail({ sessionId }: { sessionId: string }) {
  const { data: session, isLoading } = useSession(sessionId)
  const deleteSession = useDeleteSession()
  const navigate = useNavigate()

  if (isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  if (!session) {
    return <div className="text-muted-foreground">Session not found</div>
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-4">
        <Link to="/sessions"><Button variant="ghost" size="icon"><ArrowLeft className="h-4 w-4" /></Button></Link>
        <PageHeader
          className="flex-1"
          title={session.name}
          description={session.namespace}
          action={
            <Button
              variant="destructive"
              size="sm"
              onClick={async () => {
                await deleteSession.mutateAsync(session.name)
                navigate({ to: '/sessions' })
              }}
            >
              <Trash2 className="mr-2 h-4 w-4" /> Delete
            </Button>
          }
        />
      </div>

      <Tabs defaultValue="overview">
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
          <SessionEventTimeline key={session.name} sessionId={session.name} />
        </TabsContent>
      </Tabs>
    </div>
  )
}
