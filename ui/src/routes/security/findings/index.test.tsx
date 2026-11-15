import { describe, it, expect, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { createMemoryHistory, createRouter } from '@tanstack/react-router'
import { routeTree } from '@/routeTree.gen'

describe('/security/findings', () => {
  it('redirects to the Security page instead of looking up a repository named "findings"', async () => {
    const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: ['/security/findings'] }) })

    await router.load()

    expect(router.state.location.pathname).toBe('/security')
    expect(router.state.matches.map((match) => match.routeId)).not.toContain('/security/$repoId')
  })
})
