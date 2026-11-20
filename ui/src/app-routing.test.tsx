import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { delay, http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

// The app wires the router and the sign-in capture at module load, so each
// test loads it fresh against the address it starts from.
async function loadApp(url: string) {
  window.history.replaceState(null, '', url)
  vi.resetModules()
  const [{ App }, { useAuthStore }] = await Promise.all([import('./app'), import('./stores/auth')])
  return { App, useAuthStore }
}

describe('App routing', () => {
  beforeEach(() => {
    sessionStorage.clear()
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation((query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    })
  })

  it('signs in from a #token= deep link and lands on the deep link', async () => {
    // While the router moves to /login, the deep-link page still renders and
    // asks for its data without a token. Those 401s answer after the token
    // is validated and must not sign the new session out.
    server.use(http.get('/api/v1/security/repositories', async ({ request }) => {
      if (request.headers.get('Authorization') !== 'Bearer deep-link-token') {
        await delay(150)
        return HttpResponse.json({ error: { code: 401, message: 'unauthorized' } }, { status: 401 })
      }
      return HttpResponse.json({ items: [] })
    }))
    const { App, useAuthStore } = await loadApp('/security?view=all#token=deep-link-token')
    render(<App />)

    await waitFor(() => expect(useAuthStore.getState().token).toBe('deep-link-token'))
    await waitFor(() => expect(window.location.pathname).toBe('/security'))
    expect(window.location.search).toBe('?view=all')
    expect(window.location.hash).toBe('')
    expect(await screen.findByRole('heading', { level: 1, name: 'Security' })).toBeInTheDocument()
    await delay(300)
    expect(useAuthStore.getState().token).toBe('deep-link-token')
    expect(window.location.pathname).toBe('/security')
  })

  it('renders unknown addresses as a not-found page inside the layout', async () => {
    const { App, useAuthStore } = await loadApp('/no-such-page')
    useAuthStore.setState({ token: 'test-token' })
    render(<App />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Page not found' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Go to Dashboard' })).toHaveAttribute('href', '/')
    // Inside the layout: the namespace selector and sidebar render around it.
    expect(screen.getByRole('combobox', { name: 'Namespace' })).toBeInTheDocument()
    expect(screen.getByRole('main')).toContainElement(screen.getByRole('heading', { level: 1, name: 'Page not found' }))
  })
})
