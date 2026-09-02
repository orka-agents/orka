import { createFileRoute } from '@tanstack/react-router'
import { RuntimeRegistry } from '@/components/runtime/runtime-registry'

export const Route = createFileRoute('/runtimes/')({
  component: RuntimeRegistry,
})
