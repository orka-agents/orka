import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { OversightDashboard, type OversightSearch } from '@/components/oversight/oversight-dashboard'

const str = (value: unknown) => (typeof value === 'string' && value !== '' ? value : undefined)

export const Route = createFileRoute('/oversight')({
  validateSearch: (search: Record<string, unknown>): OversightSearch => ({
    q: str(search.q),
    window: str(search.window),
    decision: str(search.decision),
    view: search.view === 'needs' || search.view === 'activity' || search.view === 'matrix' || search.view === 'charter' || search.view === 'monitors'
      ? search.view
      : search.view === 'stream' ? 'activity' : undefined,
  }),
  component: OversightPage,
})

function OversightPage() {
  const search = Route.useSearch()
  const navigate = useNavigate({ from: '/oversight' })
  return (
    <OversightDashboard
      search={search}
      onSearchChange={(next) => navigate({ search: next, replace: next.q === search.q && next.window === search.window })}
    />
  )
}
