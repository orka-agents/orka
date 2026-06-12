import { Link } from '@tanstack/react-router'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Badge } from '@/components/ui/badge'
import { PageHeader } from '@/components/layout/page-header'
import { Trash2 } from 'lucide-react'
import { useSessionList, useDeleteSession } from '@/hooks/use-sessions'

function timeAgo(ts?: string): string {
  if (!ts) return '-'
  const s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

export function SessionList() {
  const { data, isLoading } = useSessionList()
  const deleteSession = useDeleteSession()

  return (
    <div className="space-y-4">
      <PageHeader title="Sessions" description="View conversation sessions" />
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Namespace</TableHead>
              <TableHead>Messages</TableHead>
              <TableHead>Tokens (in/out)</TableHead>
              <TableHead>Active Task</TableHead>
              <TableHead>Age</TableHead>
              <TableHead className="w-12"></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: 5 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 7 }).map((_, j) => (
                    <TableCell key={j}><Skeleton className="h-4 w-20" /></TableCell>
                  ))}
                </TableRow>
              ))
            ) : (data?.items ?? []).length === 0 ? (
              <TableRow>
                <TableCell colSpan={7} className="text-center text-muted-foreground py-8">
                  No sessions found.
                </TableCell>
              </TableRow>
            ) : (
              (data?.items ?? []).map((session) => (
                <TableRow key={session.name}>
                  <TableCell>
                    <Link to="/sessions/$sessionId" params={{ sessionId: session.name }} className="font-mono text-sm font-medium hover:underline">
                      {session.name}
                    </Link>
                  </TableCell>
                  <TableCell>{session.namespace}</TableCell>
                  <TableCell>{session.messageCount ?? '0'}</TableCell>
                  <TableCell>{session.inputTokens ?? '0'} / {session.outputTokens ?? '0'}</TableCell>
                  <TableCell>
                    {session.activeTask ? (
                      <Badge variant="secondary">{session.activeTask}</Badge>
                    ) : '-'}
                  </TableCell>
                  <TableCell>{timeAgo(session.createdAt)}</TableCell>
                  <TableCell>
                    <Button variant="ghost" size="icon" onClick={() => deleteSession.mutate(session.name)}>
                      <Trash2 className="h-4 w-4 text-muted-foreground" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}
