import { createFileRoute } from '@tanstack/react-router'
import { RepositoryMonitorList } from '@/components/monitors/repository-monitor-list'

export const Route = createFileRoute('/monitors/')({
  component: RepositoryMonitorList,
})
