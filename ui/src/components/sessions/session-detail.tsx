import { Link, useNavigate } from '@tanstack/react-router'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ArrowLeft, Trash2 } from 'lucide-react'
import { TranscriptViewer } from './transcript-viewer'
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
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-4">
          <Link to="/sessions"><Button variant="ghost" size="icon"><ArrowLeft className="h-4 w-4" /></Button></Link>
          <div>
            <h1 className="text-3xl font-bold tracking-tight">{session.name}</h1>
            <p className="text-muted-foreground">{session.namespace}</p>
          </div>
        </div>
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
      </div>

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
    </div>
  )
}
