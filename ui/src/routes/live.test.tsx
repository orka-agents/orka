import { describe, it, expect, vi } from 'vitest'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createFileRoute: (_path: string) => (opts: any) => ({ ...opts, path: _path }),
  }
})

import { RuntimeCanvas } from '@/components/runtime/runtime-canvas'
import { Route } from './live'

describe('/live route', () => {
  it('mounts RuntimeCanvas', () => {
    expect(Route).toBeDefined()
    expect(Route.path).toBe('/live')
    expect(Route.component).toBe(RuntimeCanvas)
  })
})
