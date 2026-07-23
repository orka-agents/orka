import { describe, it, expect, beforeEach, vi } from 'vitest'

// Track useState calls to override mode initial value
let useStateModeOverride: string | null = null

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
    useLocation: () => ({ pathname: '/agents/new' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('react', async () => {
  const actual = await vi.importActual('react')
  return {
    ...actual,
    useState: (initial: any) => {
      if (initial === 'ai' && useStateModeOverride) {
        const override = useStateModeOverride
        useStateModeOverride = null
        return (actual as any).useState(override)
      }
      return (actual as any).useState(initial)
    },
  }
})

// Polyfill ResizeObserver for jsdom (needed by Radix Select in runtime mode)
if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as any
}

if (typeof HTMLElement !== 'undefined') {
  if (!HTMLElement.prototype.hasPointerCapture) {
    HTMLElement.prototype.hasPointerCapture = () => false
  }
  if (!HTMLElement.prototype.setPointerCapture) {
    HTMLElement.prototype.setPointerCapture = () => {}
  }
  if (!HTMLElement.prototype.releasePointerCapture) {
    HTMLElement.prototype.releasePointerCapture = () => {}
  }
  if (!HTMLElement.prototype.scrollIntoView) {
    HTMLElement.prototype.scrollIntoView = () => {}
  }
}

import { fireEvent, render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { toast } from 'sonner'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { AgentCreateForm } from './agent-create-form'

describe('AgentCreateForm', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    useStateModeOverride = null
    mockNavigate.mockClear()
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
  })

  it('renders form with name and mode fields', () => {
    render(<AgentCreateForm />)
    expect(screen.getByText('Name')).toBeInTheDocument()
    expect(screen.getByText('Mode')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('my-agent')).toBeInTheDocument()
  })

  it('AI mode shows provider, model, temperature, max tokens, system prompt', () => {
    render(<AgentCreateForm />)
    expect(screen.getByText('Provider')).toBeInTheDocument()
    expect(screen.getByText('Model')).toBeInTheDocument()
    expect(screen.getByText('Temperature')).toBeInTheDocument()
    expect(screen.getByText('Max Tokens')).toBeInTheDocument()
    expect(screen.getByText('System Prompt')).toBeInTheDocument()
  })

  it('Runtime mode shows runtime type, max turns, allowed tools, allow bash switch', () => {
    useStateModeOverride = 'runtime'
    render(<AgentCreateForm />)
    expect(screen.getByText('Runtime Type')).toBeInTheDocument()
    expect(screen.getByText('Max Turns')).toBeInTheDocument()
    expect(screen.getByText('Allowed Tools')).toBeInTheDocument()
    expect(screen.getByText('Allow Bash')).toBeInTheDocument()
  })

  it('Runtime mode includes Codex as an available runtime option', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    const selects = screen.getAllByRole('combobox')
    await user.click(selects[1])
    expect(screen.getAllByText('OpenAI Codex').length).toBeGreaterThan(0)
  })

  it('Runtime mode includes OpenCode as an available runtime option', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    const selects = screen.getAllByRole('combobox')
    await user.click(selects[1])
    expect(screen.getAllByText('OpenCode').length).toBeGreaterThan(0)
  })

  it('uses restricted OpenCode tool and bash defaults', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    const selects = screen.getAllByRole('combobox')
    await user.click(selects[1])
    const opencodeOption = screen.getAllByText('OpenCode').find((element) => element.tagName !== 'OPTION')
    expect(opencodeOption).toBeDefined()
    await user.click(opencodeOption!)

    expect(screen.getByPlaceholderText('Read,Glob,LS')).toHaveValue('Read,Glob,LS')
    expect(screen.getByRole('switch')).not.toBeChecked()
    expect(screen.getByText('Max Output Tokens')).toBeInTheDocument()
    expect(screen.getByText('Context Window')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('8192 (default)')).toHaveAttribute('max', '32000')
  })

  it('secret reference select is shown', () => {
    render(<AgentCreateForm />)
    expect(screen.getByText('Secret Reference')).toBeInTheDocument()
    expect(screen.getByText('Kubernetes Secret containing API keys')).toBeInTheDocument()
  })

  it('submit button exists', () => {
    render(<AgentCreateForm />)
    const buttons = screen.getAllByText('Create Agent')
    expect(buttons.length).toBeGreaterThanOrEqual(1)
  })

  it('submits AI agent form and navigates', async () => {
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    await user.type(screen.getByPlaceholderText('my-agent'), 'test-agent')
    await user.type(screen.getByPlaceholderText('claude-opus-4-5-20250514'), 'my-model')

    // Fill in system prompt to cover line 49
    await user.type(screen.getByPlaceholderText('Optional system prompt...'), 'You are helpful')

    // Fill in max tokens
    const maxTokensInput = screen.getByPlaceholderText('Optional')
    await user.type(maxTokensInput, '4096')

    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Agent created')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/agents' })
  })

  it('shows error toast when submission fails', async () => {
    server.use(
      http.post('/api/v1/agents', () => new HttpResponse('Bad request', { status: 400 })),
    )
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    await user.type(screen.getByPlaceholderText('my-agent'), 'bad-agent')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalled()
    })
  })

  it('submits runtime agent form and navigates', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    await user.type(screen.getByPlaceholderText('my-agent'), 'runtime-agent')

    // Interact with allowed tools input to cover line 162
    const toolsInput = screen.getByPlaceholderText('Read,Glob,Grep,Bash,LS')
    await user.clear(toolsInput)
    await user.type(toolsInput, 'Read,Write')

    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Agent created')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/agents' })
  })

  it('submits the endpoint model for an OpenCode runtime agent', async () => {
    useStateModeOverride = 'runtime'
    let submitted: any
    server.use(
      http.post('/api/v1/agents', async ({ request }) => {
        submitted = await request.json()
        return HttpResponse.json({ metadata: { name: 'opencode-agent' }, spec: submitted.spec })
      }),
    )
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
    const selects = screen.getAllByRole('combobox')
    await user.click(selects[1])
    const opencodeOption = screen.getAllByText('OpenCode').find((element) => element.tagName !== 'OPTION')
    expect(opencodeOption).toBeDefined()
    await user.click(opencodeOption!)
    await user.type(screen.getByPlaceholderText('Endpoint model ID'), 'moonshotai/Kimi-K2-Instruct-0905')
    await user.type(screen.getByPlaceholderText('8192 (default)'), '4096')
    await user.type(screen.getByPlaceholderText('128000 (default)'), '64000')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Agent created')
    })
    expect(submitted.spec.runtime.type).toBe('opencode')
    expect(submitted.spec.runtime.defaultAllowBash).toBe(false)
    expect(submitted.spec.runtime.defaultAllowedTools).toEqual(['Read', 'Glob', 'LS'])
    expect(submitted.spec.model).toEqual({
      name: 'moonshotai/Kimi-K2-Instruct-0905',
      maxTokens: 4096,
      contextWindow: 64000,
    })
  })

  it('rejects a whitespace-only OpenCode model', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
    const selects = screen.getAllByRole('combobox')
    await user.click(selects[1])
    const opencodeOption = screen.getAllByText('OpenCode').find((element) => element.tagName !== 'OPTION')
    expect(opencodeOption).toBeDefined()
    await user.click(opencodeOption!)
    await user.type(screen.getByPlaceholderText('Endpoint model ID'), '   ')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    expect(toast.error).toHaveBeenCalledWith('OpenCode requires an endpoint model ID')
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('rejects a non-positive OpenCode context window', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
    const selects = screen.getAllByRole('combobox')
    await user.click(selects[1])
    const opencodeOption = screen.getAllByText('OpenCode').find((element) => element.tagName !== 'OPTION')
    expect(opencodeOption).toBeDefined()
    await user.click(opencodeOption!)
    await user.type(screen.getByPlaceholderText('Endpoint model ID'), 'kimi-k2')
    await user.type(screen.getByPlaceholderText('128000 (default)'), '0')
    fireEvent.submit(screen.getByRole('button', { name: 'Create Agent' }).closest('form')!)

    expect(toast.error).toHaveBeenCalledWith('Context Window must be a positive integer')
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('rejects an OpenCode output limit above 32000', async () => {
	useStateModeOverride = 'runtime'
	const user = userEvent.setup()
	render(<AgentCreateForm />)

	await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
	const selects = screen.getAllByRole('combobox')
	await user.click(selects[1])
	const opencodeOption = screen.getAllByText('OpenCode').find((element) => element.tagName !== 'OPTION')
	await user.click(opencodeOption!)
	await user.type(screen.getByPlaceholderText('Endpoint model ID'), 'kimi-k2')
	await user.type(screen.getByPlaceholderText('8192 (default)'), '32001')
	fireEvent.submit(screen.getByRole('button', { name: 'Create Agent' }).closest('form')!)

	expect(toast.error).toHaveBeenCalledWith('OpenCode Max Output Tokens cannot exceed 32000')
  })

  it('requires OpenCode context window to exceed effective output tokens', async () => {
	useStateModeOverride = 'runtime'
	const user = userEvent.setup()
	render(<AgentCreateForm />)

	await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
	const selects = screen.getAllByRole('combobox')
	await user.click(selects[1])
	const opencodeOption = screen.getAllByText('OpenCode').find((element) => element.tagName !== 'OPTION')
	await user.click(opencodeOption!)
	await user.type(screen.getByPlaceholderText('Endpoint model ID'), 'kimi-k2')
	await user.type(screen.getByPlaceholderText('128000 (default)'), '8192')
	fireEvent.submit(screen.getByRole('button', { name: 'Create Agent' }).closest('form')!)

	expect(toast.error).toHaveBeenCalledWith('Context Window must be greater than Max Output Tokens')
  })

  it('submits runtime agent with empty allowed tools', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)

    await user.type(screen.getByPlaceholderText('my-agent'), 'rt-agent-2')

    // Clear the allowed tools input to cover the empty-trim branch (line 56)
    const toolsInput = screen.getByPlaceholderText('Read,Glob,Grep,Bash,LS')
    await user.clear(toolsInput)

    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Agent created')
    })
  })

  it('cancel button navigates to agents', async () => {
    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/agents' })
  })
})
