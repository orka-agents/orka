import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ListTodo, Play, CheckCircle, XCircle, MessageSquare, Bot, Wrench } from 'lucide-react'
import type { Task } from '@/schemas/task'
import { Skeleton } from '@/components/ui/skeleton'

interface StatsCardsProps {
  tasks?: Task[]
  sessionCount?: number
  agentCount?: number
  toolCount?: number
  isLoading?: boolean
}

export function StatsCards({ tasks, sessionCount, agentCount, toolCount, isLoading }: StatsCardsProps) {
  if (isLoading) {
    return (
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <Card key={i}>
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <Skeleton className="h-4 w-24" />
              <Skeleton className="h-4 w-4" />
            </CardHeader>
            <CardContent>
              <Skeleton className="h-8 w-16" />
            </CardContent>
          </Card>
        ))}
      </div>
    )
  }

  const total = tasks?.length ?? 0
  const running = tasks?.filter(t => t.status?.phase === 'Running').length ?? 0
  const succeeded = tasks?.filter(t => t.status?.phase === 'Succeeded').length ?? 0
  const failed = tasks?.filter(t => t.status?.phase === 'Failed').length ?? 0

  const stats = [
    { label: 'Total Tasks', value: total, icon: ListTodo, color: 'text-foreground' },
    { label: 'Running', value: running, icon: Play, color: 'text-blue-500' },
    { label: 'Succeeded', value: succeeded, icon: CheckCircle, color: 'text-green-500' },
    { label: 'Failed', value: failed, icon: XCircle, color: 'text-red-500' },
  ]

  return (
    <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
      {stats.map(({ label, value, icon: Icon, color }) => (
        <Card key={label}>
          <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
            <CardTitle className="text-sm font-medium">{label}</CardTitle>
            <Icon className={`h-4 w-4 ${color}`} />
          </CardHeader>
          <CardContent>
            <div className="text-2xl font-bold">{value}</div>
          </CardContent>
        </Card>
      ))}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium">Sessions</CardTitle>
          <MessageSquare className="h-4 w-4 text-muted-foreground" />
        </CardHeader>
        <CardContent>
          <div className="text-2xl font-bold">{sessionCount ?? 0}</div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium">Agents</CardTitle>
          <Bot className="h-4 w-4 text-muted-foreground" />
        </CardHeader>
        <CardContent>
          <div className="text-2xl font-bold">{agentCount ?? 0}</div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium">Tools</CardTitle>
          <Wrench className="h-4 w-4 text-muted-foreground" />
        </CardHeader>
        <CardContent>
          <div className="text-2xl font-bold">{toolCount ?? 0}</div>
        </CardContent>
      </Card>
    </div>
  )
}
