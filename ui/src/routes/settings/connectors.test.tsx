import { describe, it, expect, vi } from 'vitest'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createFileRoute: (_path: string) => (opts: any) => ({ ...opts, path: _path }),
  }
})

import { Route, ConnectorsSettingsRoute } from './connectors'

describe('/settings/connectors route', () => {
  it('mounts the connectors page and keeps only the callback parameters', () => {
    expect(Route.path).toBe('/settings/connectors')
    expect(Route.component).toBe(ConnectorsSettingsRoute)
    expect((Route as any).validateSearch({ status: 'pending', connection: 'github-abc', namespace: 'team-a', reason: 7, extra: 'x' })).toEqual({
      status: 'pending', reason: undefined, connection: 'github-abc', namespace: 'team-a',
    })
  })
})
