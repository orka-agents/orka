import { createFileRoute } from '@tanstack/react-router'
import { MemoryPage } from '@/components/memory/memory-page'

export const Route = createFileRoute('/memory')({
  component: MemoryPage,
})
