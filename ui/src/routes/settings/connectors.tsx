import { createFileRoute } from '@tanstack/react-router'
import { ConnectorsPage } from '@/components/connectors/connectors-page'
import type { ConnectorCallbackSearch } from '@/lib/connectors'

export const Route = createFileRoute('/settings/connectors')({
  validateSearch: (search: Record<string, unknown>): ConnectorCallbackSearch => ({
    status: typeof search.status === 'string' ? search.status : undefined,
    reason: typeof search.reason === 'string' ? search.reason : undefined,
    connection: typeof search.connection === 'string' ? search.connection : undefined,
  }),
  component: ConnectorsSettingsRoute,
})

export function ConnectorsSettingsRoute() {
  const search = Route.useSearch()
  return <ConnectorsPage search={search} />
}
