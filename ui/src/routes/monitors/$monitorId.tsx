import { createFileRoute } from '@tanstack/react-router'
import { RepositoryMonitorDetail } from '@/components/monitors/repository-monitor-detail'

export const Route = createFileRoute('/monitors/$monitorId')({
  component: MonitorDetailPage,
})

function MonitorDetailPage() {
  const { monitorId } = Route.useParams()
  return <RepositoryMonitorDetail monitorName={monitorId} />
}
