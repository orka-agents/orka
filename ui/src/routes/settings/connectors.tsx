import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { ConnectorsPage } from '@/components/connectors/connectors-page'
import type { ConnectorCallbackSearch } from '@/lib/connectors'

export const Route = createFileRoute('/settings/connectors')({
  validateSearch: (search: Record<string, unknown>): ConnectorCallbackSearch => ({
    status: typeof search.status === 'string' ? search.status : undefined,
    reason: typeof search.reason === 'string' ? search.reason : undefined,
    connection: typeof search.connection === 'string' ? search.connection : undefined,
    namespace: typeof search.namespace === 'string' ? search.namespace : undefined,
  }),
  component: ConnectorsSettingsRoute,
})

export function ConnectorsSettingsRoute() {
  const search = Route.useSearch()
  const navigate = useNavigate()
  // The spent callback leaves the router's search state too, not only the
  // address bar, so a remount (a namespace switch) never replays it.
  const clearCallback = () => { void navigate({ to: '/settings/connectors', search: {}, replace: true }) }
  return <ConnectorsPage search={search} clearCallback={clearCallback} />
}
