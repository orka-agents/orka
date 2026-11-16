import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/agents' }),
  }
})

import { render, screen, waitFor } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { AgentList } from './agent-list'

describe('AgentList', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('loading state shows skeleton cards', () => {
    const { container } = render(<AgentList />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('empty state shows "No agents registered."', async () => {
    render(<AgentList />)
    await waitFor(() => {
      expect(screen.getByText('No agents registered.')).toBeInTheDocument()
    })
  })

  it('populated grid shows agent cards', async () => {
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'agent-a', namespace: 'default', uid: 'uid-a' },
              spec: {
                model: { provider: 'anthropic', name: 'claude-sonnet-4-20250514' },
                tools: [{ name: 'tool1', enabled: true }],
              },
              status: { activeTasks: 3 },
            },
            {
              metadata: { name: 'agent-b', namespace: 'default', uid: 'uid-b' },
              spec: { runtime: { type: 'codex' } },
              status: { activeTasks: 0 },
            },
            {
              metadata: { name: 'agent-c', namespace: 'default', uid: 'uid-c' },
              spec: { model: { name: 'openai/gpt-5.4' }, runtime: { type: 'opencode' } },
              status: { activeTasks: 0 },
            },
          ],
          metadata: {},
        })
      )
    )

    render(<AgentList />)

    await waitFor(() => {
      expect(screen.getByText('agent-a')).toBeInTheDocument()
    })

    // Agent A: name, namespace, provider badge, model badge, tools count
    expect(screen.getByText('anthropic')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet-4-20250514')).toBeInTheDocument()
    expect(screen.getByText('1 tool')).toBeInTheDocument()
    expect(screen.getByText('Active: 3')).toBeInTheDocument()

    // Agent B: name, runtime badge
    expect(screen.getByText('agent-b')).toBeInTheDocument()
    expect(screen.getByText('Codex ACP')).toBeInTheDocument()

    // Agent C: OpenCode keeps its branded runtime label.
    expect(screen.getByText('agent-c')).toBeInTheDocument()
    expect(screen.getByText('OpenCode ACP')).toBeInTheDocument()
  })

  it('shows the providerRef name and a readiness badge from the Ready condition', async () => {
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'ref-agent', namespace: 'default', uid: 'uid-r' },
              spec: { providerRef: { name: 'anthropic-prod' }, model: { provider: 'anthropic' } },
              status: { activeTasks: 0, conditions: [{ type: 'Ready', status: 'True' }] },
            },
            {
              metadata: { name: 'broken-agent', namespace: 'default', uid: 'uid-b' },
              spec: { providerRef: { name: 'missing' } },
              status: { activeTasks: 0, conditions: [{ type: 'Ready', status: 'False', message: 'referenced provider "missing" not found' }] },
            },
            {
              metadata: { name: 'new-agent', namespace: 'default', uid: 'uid-n' },
              spec: { providerRef: { name: 'anthropic-prod' } },
            },
          ],
          metadata: {},
        })
      )
    )

    render(<AgentList />)
    await waitFor(() => expect(screen.getByText('ref-agent')).toBeInTheDocument())
    expect(screen.getAllByText('anthropic-prod')).toHaveLength(2)
    expect(screen.queryByText('anthropic')).not.toBeInTheDocument()
    expect(screen.getByText('missing')).toBeInTheDocument()
    expect(screen.getByText('Ready')).toBeInTheDocument()
    expect(screen.getByText('Not ready')).toHaveAttribute('title', 'referenced provider "missing" not found')
  })

  it('keeps long agent names readable at tablet widths', async () => {
    const longName = 'repository-security-reviewer-agent'
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: longName, namespace: 'orka-system', uid: 'uid-long' },
              spec: {},
              status: { activeTasks: 0, conditions: [{ type: 'Ready', status: 'True' }] },
            },
          ],
          metadata: {},
        })
      )
    )

    render(<AgentList />)
    const title = await screen.findByText(longName)
    // A truncated name keeps its full text available as a tooltip.
    expect(title).toHaveAttribute('title', longName)
    expect(screen.getByText('orka-system')).toHaveAttribute('title', 'orka-system')
    // Two columns only from lg and three from xl, so md cards span the full width.
    const grid = title.closest('.grid')
    expect(grid).toHaveClass('lg:grid-cols-2', 'xl:grid-cols-3')
    expect(grid).not.toHaveClass('md:grid-cols-2')
    expect(grid).not.toHaveClass('lg:grid-cols-3')
    // The readiness badge sits under the name instead of beside it in the header row.
    const nameColumn = title.parentElement as HTMLElement
    expect(nameColumn).toContainElement(screen.getByText('Ready'))
  })

  it('agent card shows namespace', async () => {
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'my-agent', namespace: 'production', uid: 'uid-1' },
              spec: {},
              status: { activeTasks: 0 },
            },
          ],
          metadata: {},
        })
      )
    )

    render(<AgentList />)
    await waitFor(() => {
      expect(screen.getByText('my-agent')).toBeInTheDocument()
    })
    expect(screen.getByText('production')).toBeInTheDocument()
  })

  it('"New Agent" button links to /agents/new', () => {
    render(<AgentList />)
    const link = screen.getByText('New Agent').closest('a')
    expect(link).toHaveAttribute('href', '/agents/new')
  })
})
