import { createFileRoute } from '@tanstack/react-router'
import { UsagePage } from '@/components/usage/usage-page'

export const Route = createFileRoute('/usage')({ component: UsagePage })
