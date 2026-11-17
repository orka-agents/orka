import { describe, expect, it, vi } from 'vitest'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createFileRoute: (_path: string) => (opts: any) => ({ ...opts, path: _path }),
  }
})

import { Route } from './kanban'
import { Route as ApprovalsRoute } from './approvals'
import { ApprovalsInbox } from '@/components/approvals/approvals-inbox'

describe('/kanban and /approvals routes', () => {
  it('accepts only the attention and phase board views', () => {
    const validateSearch = (Route as any).validateSearch as (search: Record<string, unknown>) => { view?: string }
    expect(Route.path).toBe('/kanban')
    expect(validateSearch({ view: 'attention' })).toEqual({ view: 'attention' })
    expect(validateSearch({ view: 'phase' })).toEqual({ view: 'phase' })
    expect(validateSearch({ view: 'bogus' })).toEqual({ view: 'phase' })
    expect(validateSearch({})).toEqual({ view: undefined })
  })

  it('mounts the approvals inbox', () => {
    expect(ApprovalsRoute.path).toBe('/approvals')
    expect((ApprovalsRoute as any).component).toBe(ApprovalsInbox)
  })
})
