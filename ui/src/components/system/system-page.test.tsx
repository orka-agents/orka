import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { SystemPage } from './system-page'

describe('SystemPage', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    server.use(
      http.get('/readyz', () =>
        HttpResponse.json({ status: 'ok', checks: { store: 'ok', kubernetes: 'ok' } }),
      ),
      http.get('/openai/v1/models', () =>
        HttpResponse.json({
          object: 'list',
          data: [
            { id: 'anthropic/claude-sonnet-4', object: 'model' },
            { id: 'claude-sonnet-4', object: 'model' },
          ],
        }),
      ),
    )
  })

  it('shows readiness checks and capability badges', async () => {
    render(<SystemPage />)
    await waitFor(() => expect(screen.getByText('API: ok')).toBeInTheDocument())
    expect(screen.getByText('store: ok')).toBeInTheDocument()
    expect(screen.getByText('kubernetes: ok')).toBeInTheDocument()
    // Chat is enabled in the default mock config; memory store defaults to ok.
    await waitFor(() => expect(screen.getByText('chat: ok')).toBeInTheDocument())
    await waitFor(() => expect(screen.getByText('memory store: ok')).toBeInTheDocument())
  })

  it('marks the memory store unhealthy on 501', async () => {
    server.use(
      http.get('/api/v1/memories', () =>
        HttpResponse.json({ error: { code: 501, message: 'not configured' } }, { status: 501 }),
      ),
    )
    render(<SystemPage />)
    await waitFor(() => expect(screen.getByText('memory store: unhealthy')).toBeInTheDocument())
  })

  it('lists compat endpoints and the model catalog', async () => {
    render(<SystemPage />)
    expect(await screen.findByText('OpenAI-compatible', { selector: 'p' })).toBeInTheDocument()
    expect(screen.getByText('Anthropic-compatible', { selector: 'p' })).toBeInTheDocument()
    await waitFor(() => expect(screen.getByText('anthropic/claude-sonnet-4')).toBeInTheDocument())
  })

  it('shows chat orchestrator limits from chat config', async () => {
    render(<SystemPage />)
    await waitFor(() => expect(screen.getByText(/chat orchestrator/i)).toBeInTheDocument())
    expect(screen.getByText(/anthropic\/claude-sonnet-4-20250514/)).toBeInTheDocument()
  })
})
