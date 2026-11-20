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
    runtime: { type: 'claude' },
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

  it('shows a styled not-found page with a way back when the API returns 404', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () =>
        HttpResponse.json({ error: { code: 404, message: 'agent not found' } }, { status: 404 }),
      ),
    )
    render(<AgentDetail agentId="nonexistent" />)
    expect(await screen.findByRole('heading', { level: 1, name: 'Agent not found' })).toBeInTheDocument()
    expect(screen.getByText('No agent named "nonexistent"')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to agents' })).toHaveAttribute('href', '/agents')
    expect(document.title).toBe('Agent not found · Orka')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument()
  })

  it('shows a not-authorized state, not "Agent not found", on 403', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 }),
      ),
    )
    render(<AgentDetail agentId="secret-agent" />)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Not authorized to view agents')
    expect(alert).toHaveTextContent('read permission (not authorized)')
    expect(screen.getByRole('heading', { level: 1, name: 'Could not load agent' })).toBeInTheDocument()
    expect(screen.queryByText('Agent not found')).not.toBeInTheDocument()
  })

  it('shows a load error, not "Agent not found", for other failures', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () =>
        HttpResponse.json({ error: { code: 500, message: 'apiserver unavailable' } }, { status: 500 }),
      ),
    )
    render(<AgentDetail agentId="broken-agent" />)
    expect(await screen.findByRole('alert')).toHaveTextContent('apiserver unavailable')
    expect(screen.getByRole('link', { name: 'Back to agents' })).toHaveAttribute('href', '/agents')
    expect(screen.queryByText('Agent not found')).not.toBeInTheDocument()
  })

  it('names the back link to the agent list and nests no button in it', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent))
    )
    render(<AgentDetail agentId="test-agent" />)
    const back = await screen.findByRole('link', { name: 'Back to agents' })
    expect(back).toHaveAttribute('href', '/agents')
    expect(back.querySelector('button')).toBeNull()
    expect(back.closest('button')).toBeNull()
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
      expect(screen.getByText('Agent Runtime')).toBeInTheDocument()
    })
    expect(screen.getByText('Claude ACP')).toBeInTheDocument()
    expect(screen.getByText('Orka-managed RuntimePool')).toBeInTheDocument()
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
    // Readiness badge plus the Ready condition row.
    expect(screen.getAllByText('Ready')).toHaveLength(2)
  })

  it('shows the providerRef and a Not ready badge with the condition message', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json({
        metadata: { name: 'ref-agent', namespace: 'default' },
        spec: { providerRef: { name: 'anthropic-prod' } },
        status: {
          activeTasks: 0,
          conditions: [{ type: 'Ready', status: 'False', message: 'referenced provider "anthropic-prod" is not ready' }],
        },
      })),
    )
    render(<AgentDetail agentId="ref-agent" />)
    await waitFor(() => expect(screen.getByText('Model Configuration')).toBeInTheDocument())
    expect(screen.getByText('anthropic-prod')).toBeInTheDocument()
    expect(screen.getByText('(Provider resource)')).toBeInTheDocument()
    expect(screen.getByText('Not ready')).toBeInTheDocument()
    expect(screen.getAllByText('referenced provider "anthropic-prod" is not ready').length).toBeGreaterThanOrEqual(1)
  })

  it('shows coordination autonomy and runtime defaults', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json({
        metadata: { name: 'coord-agent', namespace: 'default' },
        spec: {
          runtime: {
            type: 'codex',
            contractVersion: 'orka.harness.v2',
            defaultMaxTurns: 40,
            defaultAllowedTools: ['Read', 'Grep'],
          },
          coordination: {
            enabled: true,
            autonomous: true,
            maxIterations: 7,
            approvalRequiredTools: ['deploy_release'],
          },
        },
        status: { activeTasks: 0 },
      })),
    )
    render(<AgentDetail agentId="coord-agent" />)
    await waitFor(() => expect(screen.getByText('Agent Runtime')).toBeInTheDocument())
    expect(screen.getByText('Codex ACP')).toBeInTheDocument()
    expect(screen.getByText('orka.harness.v2')).toBeInTheDocument()
    expect(screen.getByText('40')).toBeInTheDocument()
    expect(screen.getByText('Read')).toBeInTheDocument()
    expect(screen.getByText('Grep')).toBeInTheDocument()
    expect(screen.getByText('Autonomous:').parentElement).toHaveTextContent('Autonomous: Yes')
    expect(screen.getByText('Max Iterations:').parentElement).toHaveTextContent('Max Iterations: 7')
    expect(screen.getByText('deploy_release')).toBeInTheDocument()
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

  it('keeps keyboard focus on Delete while a refused delete is in flight', async () => {
    const user = userEvent.setup()
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    let deletes = 0
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json(fullAgent)),
      http.delete('/api/v1/agents/:name', async () => {
        deletes += 1
        await gate
        return HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })
      }),
    )
    render(<AgentDetail agentId="test-agent" />)
    const button = await screen.findByRole('button', { name: /delete/i })
    button.focus()
    await user.keyboard('{Enter}')

    // Pending: marked unavailable but never disabled, so focus stays put.
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
    expect(button).not.toBeDisabled()
    expect(button).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(deletes).toBe(1)

    release()
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Failed to delete agent: not authorized'))
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled'))
    expect(button).toHaveFocus()
    expect(mockNavigate).not.toHaveBeenCalled()
    confirmSpy.mockRestore()
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

  it('labels a legacy zero maxTokens instead of rendering a stray 0', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json({
        ...fullAgent,
        spec: { ...fullAgent.spec, model: { ...fullAgent.spec.model, maxTokens: 0 } },
      })),
    )
    render(<AgentDetail agentId="test-agent" />)
    const label = await screen.findByText('Max Tokens:')
    expect(label.parentElement).toHaveTextContent('Max Tokens: 0')
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
      http.get('/api/v1/agents/:name', () => HttpResponse.json(agentWithCMPrompt)),
      http.get('/api/v1/agents/:name/system-prompt', () =>
        HttpResponse.json({ configMapRef: { name: 'my-cm', key: 'prompt.md' }, content: 'Prompt from the ConfigMap.' })),
    )
    render(<AgentDetail agentId="test-agent" />)
    expect(await screen.findByRole('region', { name: 'System prompt' })).toHaveTextContent('Prompt from the ConfigMap.')
    expect(screen.getByText('my-cm')).toBeInTheDocument()
    expect(screen.getByText('prompt.md')).toBeInTheDocument()
  })

  it('names the ConfigMap but not its content when the caller may not read it', async () => {
    server.use(
      http.get('/api/v1/agents/:name', () => HttpResponse.json({
        ...fullAgent,
        spec: { ...fullAgent.spec, systemPrompt: { configMapRef: { name: 'my-cm', key: 'prompt.md' } } },
      })),
      http.get('/api/v1/agents/:name/system-prompt', () =>
        HttpResponse.json({ error: { code: 403, message: 'not authorized' } }, { status: 403 })),
    )
    render(<AgentDetail agentId="test-agent" />)
    expect(await screen.findByText('You are not allowed to read this ConfigMap.')).toBeInTheDocument()
    expect(screen.getByText('my-cm')).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'System prompt' })).not.toBeInTheDocument()
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
