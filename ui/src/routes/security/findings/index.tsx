import { createFileRoute, redirect } from '@tanstack/react-router'

export const Route = createFileRoute('/security/findings/')({
  beforeLoad: () => {
    throw redirect({ to: '/security', replace: true })
  },
})
