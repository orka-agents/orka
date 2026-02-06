import { createFileRoute } from '@tanstack/react-router'
import { ToolDetail } from '@/components/tools/tool-detail'

export const Route = createFileRoute('/tools/$toolName')({
  component: ToolDetailPage,
})

function ToolDetailPage() {
  const { toolName } = Route.useParams()
  return <ToolDetail toolName={toolName} />
}
