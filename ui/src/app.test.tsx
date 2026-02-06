import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createRouter: () => ({}),
    RouterProvider: () => <div data-testid="router">Router</div>,
  }
})

vi.mock('./routeTree.gen', () => ({
  routeTree: {},
}))

import { App } from './app'

describe('App', () => {
  it('renders without crashing', () => {
    render(<App />)
    expect(screen.getByTestId('router')).toBeInTheDocument()
  })
})
