import { createFileRoute, redirect } from '@tanstack/react-router'

// Findings are listed per repository, so the bare collection address goes to
// the Security page instead of matching /security/$repoId as a repository
// named "findings".
export const Route = createFileRoute('/security/findings/')({
  beforeLoad: () => {
    throw redirect({ to: '/security', replace: true })
  },
})
