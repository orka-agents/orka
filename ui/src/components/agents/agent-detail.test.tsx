import { describe, it, expect, beforeEach, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockNavigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => mockNavigate,
    useLocation: () => ({ pathname: '/agents' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { render, screen, waitFor } from '@/test/test-utils'
import { toast } from 'sonner'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { AgentDetail } from './agent-detail'

const fullAgent = {
  metadata: { name: 'test-agent', namespace: 'default', uid: 'uid-1' },
  spec: {
    model: { provider: 'anthropic', name: 'claude-sonnet-4-20250514', temperature: 0.7 },
    runtime: { type: 'claude', defaultMaxTurns: 50, defaultAllowBash: true, defaultAllowedTools: ['Read', 'Grep'] },
    tools: [{ name: 'create_task', enabled: true }, { name: 'list_tasks', enabled: false }],
    systemPrompt: { inline: 'You are a helpful agent.' },
    coordination: { enabled: true, maxConcurrentChildren: 3, maxDepth: 2, allowedAgents: [{ name: 'helper' }] },
  },
  status: { activeTasks: 2, lastUsed: '2025-01-01T00:00:00Z', conditions: [{ type: 'Ready', status: 'True' }] },
}

describe('AgentDetail', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    mockNavigate.mockClear()
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
  })

  it('loading state shows skeletons', () => {
    const { container } = render(<AgentDetail agentId="test-agent" />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('not found shows message', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(null))
    )
    render(<AgentDetail agentId="nonexistent" />)
    await waitFor(() => {
      expect(screen.getByText('Agent not found')).toBeInTheDocument()
    })
  })

  it('shows agent name and namespace', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('test-agent')).toBeInTheDocument()
    })
    expect(screen.getByText('default')).toBeInTheDocument()
  })

  it('shows model configuration card', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Model Configuration')).toBeInTheDocument()
    })
    expect(screen.getByText('anthropic')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet-4-20250514')).toBeInTheDocument()
    expect(screen.getByText('0.7')).toBeInTheDocument()
  })

  it('shows runtime card', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('CLI Runtime')).toBeInTheDocument()
    })
    expect(screen.getByText('claude')).toBeInTheDocument()
    expect(screen.getByText('50')).toBeInTheDocument()
    // "Allow Bash: Yes" - use getAllByText since coordination also shows "Yes"
    expect(screen.getAllByText('Yes').length).toBeGreaterThanOrEqual(1)
    expect(screen.getByText('Read')).toBeInTheDocument()
    expect(screen.getByText('Grep')).toBeInTheDocument()
  })

  it('shows status card with active tasks', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Status')).toBeInTheDocument()
    })
    expect(screen.getAllByText('2').length).toBeGreaterThanOrEqual(1)
    expect(screen.getByText('Ready')).toBeInTheDocument()
  })

  it('shows tools card', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Tools')).toBeInTheDocument()
    })
    expect(screen.getByText('create_task')).toBeInTheDocument()
    expect(screen.getByText('list_tasks (disabled)')).toBeInTheDocument()
  })

  it('shows system prompt', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('System Prompt')).toBeInTheDocument()
    })
    expect(screen.getByText('You are a helpful agent.')).toBeInTheDocument()
  })

  it('shows coordination card', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Coordination')).toBeInTheDocument()
    })
    expect(screen.getByText('3')).toBeInTheDocument()
    expect(screen.getByText('helper')).toBeInTheDocument()
  })

  it('delete button exists', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })
  })

  it('delete button with confirm calls delete and navigates', async () => {
    const user = userEvent.setup()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })
    await user.click(screen.getByRole('button', { name: /delete/i }))
    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Agent deleted')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/agents' })
    vi.mocked(window.confirm).mockRestore()
  })

  it('delete cancelled does nothing', async () => {
    const user = userEvent.setup()
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })
    await user.click(screen.getByRole('button', { name: /delete/i }))
    expect(mockNavigate).not.toHaveBeenCalled()
    expect(toast.success).not.toHaveBeenCalled()
    vi.mocked(window.confirm).mockRestore()
  })

  it('shows error toast when delete fails', async () => {
    const user = userEvent.setup()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent)),
      http.delete('/api/v1/agents/:name', () => new HttpResponse('Server error', { status: 500 })),
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })
    await user.click(screen.getByRole('button', { name: /delete/i }))
    await waitFor(() => {
      expect(toast.error).toHaveBeenCalled()
    })
    vi.mocked(window.confirm).mockRestore()
  })

  it('renders agent with maxTokens in model', async () => {
    const agentWithMaxTokens = {
      ...fullAgent,
      spec: {
        ...fullAgent.spec,
        model: { ...fullAgent.spec.model, maxTokens: 4096 },
      },
    }
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(agentWithMaxTokens))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('4096')).toBeInTheDocument()
    })
  })

  it('renders agent with configMapRef system prompt', async () => {
    const agentWithCMPrompt = {
      ...fullAgent,
      spec: {
        ...fullAgent.spec,
        systemPrompt: { configMapRef: { name: 'my-cm', key: 'prompt.md' } },
      },
    }
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(agentWithCMPrompt))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('ConfigMap: my-cm/prompt.md')).toBeInTheDocument()
    })
  })

  it('renders agent with conditions that have messages', async () => {
    const agentWithCondMsg = {
      ...fullAgent,
      status: {
        ...fullAgent.status,
        conditions: [{ type: 'Ready', status: 'False', message: 'Not ready yet' }],
      },
    }
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(agentWithCondMsg))
    )
    render(<AgentDetail agentId="test-agent" />)
    await waitFor(() => {
      expect(screen.getByText('— Not ready yet')).toBeInTheDocument()
    })
  })
})
