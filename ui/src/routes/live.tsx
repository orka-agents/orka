import { createFileRoute } from '@tanstack/react-router'
import { RuntimeCanvas } from '@/components/runtime/runtime-canvas'

export const Route = createFileRoute('/live')({
  component: RuntimeCanvas,
})
