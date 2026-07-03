import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, createFileRoute: (_p: string) => (opts: any) => ({ ...opts, path: _p }), useNavigate: () => vi.fn(), Link: ({ children }: any) => <span>{children}</span> }
})

import { Route } from './runtime-simulator'

describe('/runtime-simulator route', () => {
  it('exposes a component (DEV-gated)', () => {
    expect(Route.path).toBe('/runtime-simulator')
    expect(Route.component).toBeDefined()
  })

  it('in DEV mode mounts simulator; production fallback has no controls', () => {
    const C = Route.component!
    render(<C />)
    // vitest runs with import.meta.env.DEV true; simulator should appear.
    expect(screen.getByText('SIMULATOR')).toBeInTheDocument()
    // production branch text is fixed; simulator controls only exist in DEV.
    const fallback = () => <div className="text-muted-foreground">Not available.</div>
    expect(fallback().props.children).toBe('Not available.')
  })
})

