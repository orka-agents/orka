import { createFileRoute } from '@tanstack/react-router'
import { ApprovalsInbox } from '@/components/approvals/approvals-inbox'

export const Route = createFileRoute('/approvals')({
  component: ApprovalsInbox,
})
