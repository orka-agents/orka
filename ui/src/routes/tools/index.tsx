import { createFileRoute } from '@tanstack/react-router'
import { ToolList } from '@/components/tools/tool-list'

export const Route = createFileRoute('/tools/')({
  component: ToolList,
})
