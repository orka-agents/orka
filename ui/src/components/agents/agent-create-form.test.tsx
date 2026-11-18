import { describe, it, expect, beforeEach, vi } from 'vitest'

let useStateModeOverride: string | null = null

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

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

import { render, screen, waitFor, fireEvent, act } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { toast } from 'sonner'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { AgentCreateForm } from './agent-create-form'

function installDOMPolyfills() {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class { observe() {}; unobserve() {}; disconnect() {} } as any
  }
  if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false
  if (!HTMLElement.prototype.setPointerCapture) HTMLElement.prototype.setPointerCapture = () => {}
  if (!HTMLElement.prototype.releasePointerCapture) HTMLElement.prototype.releasePointerCapture = () => {}
  if (!HTMLElement.prototype.scrollIntoView) HTMLElement.prototype.scrollIntoView = () => {}
}

const providerItems = [
  { metadata: { name: 'anthropic-prod', namespace: 'default' }, spec: { type: 'anthropic', defaultModel: 'claude-default' }, status: { ready: true } },
  { metadata: { name: 'openai-lab', namespace: 'default' }, spec: { type: 'openai', defaultModel: 'gpt-default' }, status: { ready: false } },
]

async function pickOption(trigger: Element, name: string | RegExp) {
  await act(async () => {
    fireEvent.pointerDown(trigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
  })
  const option = await screen.findByRole('option', { name })
  await act(async () => {
    fireEvent.click(option)
  })
}

describe('AgentCreateForm', () => {
  beforeEach(() => {
    installDOMPolyfills()
    server.use(http.get('/api/v1/providers', () => HttpResponse.json({ items: providerItems, metadata: {} })))
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    useStateModeOverride = null
    mockNavigate.mockClear()
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
  })

  it('preserves native AI configuration fields', () => {
    render(<AgentCreateForm />)
    expect(screen.getAllByText('Native AI (LLM provider)').length).toBeGreaterThan(0)
    expect(screen.getByText('Provider')).toBeInTheDocument()
    expect(screen.getByText('Temperature')).toBeInTheDocument()
    expect(screen.getByText('Max Tokens')).toBeInTheDocument()
    expect(screen.getByText('Secret Reference')).toBeInTheDocument()
    expect(screen.getByLabelText('System Prompt')).not.toBeRequired()
    expect(screen.getByLabelText('Soul (optional)')).toBeInTheDocument()
  })

  it('shows all built-in ACP runtime options without legacy loop controls', async () => {
    useStateModeOverride = 'runtime'
    const user = userEvent.setup()
    render(<AgentCreateForm />)
    expect(screen.getByText('Runtime source')).toBeInTheDocument()
    expect(screen.getByText('Runtime profile')).toBeInTheDocument()
    expect(screen.getAllByText('Orka-managed RuntimePool').length).toBeGreaterThan(0)
    expect(screen.getByLabelText('Model')).toBeRequired()
    expect(screen.getByLabelText('Model')).toHaveAttribute('placeholder', 'Enter model identifier')
    expect(screen.queryByText('Max Turns')).not.toBeInTheDocument()
    expect(screen.queryByText('Allowed Tools')).not.toBeInTheDocument()
    expect(screen.queryByText('Allow Bash')).not.toBeInTheDocument()

    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await user.click(profileTrigger)
    expect(await screen.findByRole('option', { name: 'Claude ACP' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'OpenAI Codex ACP' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'GitHub Copilot ACP' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'OpenCode ACP' })).toBeInTheDocument()
  })

  it('submits a built-in ACP runtime profile', async () => {
    useStateModeOverride = 'runtime'
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'runtime-agent')
    await user.type(screen.getByLabelText('Model'), 'claude-sonnet-4-20250514')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.runtime).toEqual({ type: 'claude' })
    expect(submitted.spec.model).toEqual({ name: 'claude-sonnet-4-20250514' })
    expect(submitted.spec.runtime.defaultMaxTurns).toBeUndefined()
    expect(submitted.spec.secretRef).toBeUndefined()
    expect(submitted.spec.systemPrompt).toBeUndefined()
    expect(submitted.spec.soul).toBeUndefined()
  })

  it.each([
    ['claude', 'Claude ACP'],
    ['codex', 'OpenAI Codex ACP'],
    ['copilot', 'GitHub Copilot ACP'],
    ['opencode', 'OpenCode ACP'],
  ])('submits separate role and soul for the built-in %s runtime', async (runtimeType, profileLabel) => {
    useStateModeOverride = 'runtime'
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'runtime-agent')
    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(profileTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    fireEvent.click(await screen.findByRole('option', { name: profileLabel }))
    await waitFor(() => expect(profileTrigger).toHaveTextContent(profileLabel))
    await user.type(screen.getByLabelText('Model'), runtimeType === 'opencode' ? 'openai/gpt-5.4' : 'runtime-model')
    if (runtimeType === 'opencode') {
      await user.type(screen.getByLabelText('Context Window'), '32768')
      await user.type(screen.getByLabelText('Max Output Tokens'), '4096')
    }
    expect(screen.getByLabelText('System Prompt')).not.toBeRequired()
    const roleText = runtimeType === 'copilot' ? 'Review code for correctness.' : 'Discuss @mentions literally.'
    const soulText = runtimeType === 'copilot' ? 'Be concise and respectful.' : 'Sign off as @owl.'
    await user.type(screen.getByLabelText('System Prompt'), roleText)
    await user.type(screen.getByLabelText('Soul (optional)'), soulText)
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.runtime.type).toBe(runtimeType)
    expect(submitted.spec.systemPrompt).toEqual({ inline: roleText })
    expect(submitted.spec.soul).toEqual({ inline: soulText })
  })

  it('keeps the AI role visible and editable when switching to a built-in runtime', async () => {
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'runtime-agent')
    await user.type(screen.getByLabelText('System Prompt'), 'Original AI role')
    await user.type(screen.getByLabelText('Soul (optional)'), 'Shared persona')
    const modeTrigger = screen.getByText('Mode').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(modeTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    fireEvent.click(await screen.findByRole('option', { name: 'ACP agent runtime' }))
    // The model is mode-specific and starts over on a mode switch.
    await user.type(screen.getByLabelText('Model'), 'runtime-model')

    const roleInput = screen.getByLabelText('System Prompt')
    expect(roleInput).toHaveValue('Original AI role')
    await user.clear(roleInput)
    await user.type(roleInput, 'Updated runtime role')
    expect(screen.getByLabelText('Soul (optional)')).toHaveValue('Shared persona')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.runtime).toEqual({ type: 'claude' })
    expect(submitted.spec.systemPrompt).toEqual({ inline: 'Updated runtime role' })
    expect(submitted.spec.soul).toEqual({ inline: 'Shared persona' })
  })

  it.each(['System Prompt', 'Soul (optional)'])('rejects Copilot import markers in %s before posting', async (fieldLabel) => {
    useStateModeOverride = 'runtime'
    let postCount = 0
    server.use(http.post('/api/v1/agents', () => {
      postCount += 1
      return HttpResponse.json({})
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'copilot-agent')
    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(profileTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    fireEvent.click(await screen.findByRole('option', { name: 'GitHub Copilot ACP' }))
    await waitFor(() => expect(profileTrigger).toHaveTextContent('GitHub Copilot ACP'))
    await user.type(screen.getByLabelText('Model'), 'runtime-model')
    await user.type(screen.getByLabelText(fieldLabel), 'Follow @instructions.md')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    expect(toast.error).toHaveBeenCalledWith('Copilot instructions must not contain @ references; inline the referenced text')
    expect(postCount).toBe(0)
    expect(toast.success).not.toHaveBeenCalled()
    expect(mockNavigate).not.toHaveBeenCalled()
  })

  it('submits a built-in Copilot ACP runtime profile', async () => {
    useStateModeOverride = 'runtime'
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'copilot-agent')
    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(profileTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    const copilotOption = await screen.findByRole('option', { name: 'GitHub Copilot ACP' })
    await act(async () => {
      fireEvent.click(copilotOption)
    })
    await waitFor(() => expect(profileTrigger).toHaveTextContent('GitHub Copilot ACP'))
    await user.type(screen.getByLabelText('Model'), 'gpt-5.3-codex')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.runtime).toEqual({ type: 'copilot' })
    expect(submitted.spec.model).toEqual({ name: 'gpt-5.3-codex' })
  })

  it('submits OpenCode with its reviewed defaults and required provider/model ID', async () => {
    useStateModeOverride = 'runtime'
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(profileTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    const opencodeOption = await screen.findByRole('option', { name: 'OpenCode ACP' })
    await act(async () => {
      fireEvent.click(opencodeOption)
    })
    await waitFor(() => expect(profileTrigger).toHaveTextContent('OpenCode ACP'))

    const modelInput = screen.getByLabelText('Model')
    expect(modelInput).toBeRequired()
    expect(modelInput).toHaveAttribute('placeholder', 'provider/model')
    expect(screen.getByText('OpenCode model IDs use provider/model form.')).toBeInTheDocument()
    expect(screen.getByText(/Reviewed native-tool defaults: Read, Write, Edit, Bash, Glob, and Grep/)).toBeInTheDocument()
    await user.type(modelInput, 'openai/gpt-5.4')
    await user.type(screen.getByLabelText('Context Window'), '32768')
    await user.type(screen.getByLabelText('Max Output Tokens'), '4096')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.runtime).toEqual({
      type: 'opencode',
      defaultAllowedTools: ['Read', 'Write', 'Edit', 'Bash', 'Glob', 'Grep'],
      defaultAllowBash: true,
    })
    expect(submitted.spec.model).toEqual({ name: 'openai/gpt-5.4', contextWindow: 32768, maxTokens: 4096 })
  })

  it('rejects OpenCode when reviewed model limits are missing', async () => {
    useStateModeOverride = 'runtime'
    let postCount = 0
    server.use(http.post('/api/v1/agents', () => {
      postCount += 1
      return HttpResponse.json({})
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(profileTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    fireEvent.click(await screen.findByRole('option', { name: 'OpenCode ACP' }))
    await user.type(screen.getByLabelText('Model'), 'openai/gpt-5.4')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    expect(toast.error).toHaveBeenCalledWith('OpenCode requires a positive context window')
    expect(postCount).toBe(0)
  })

  it('rejects a bare OpenCode model name before posting', async () => {
    useStateModeOverride = 'runtime'
    let postCount = 0
    server.use(http.post('/api/v1/agents', () => {
      postCount += 1
      return HttpResponse.json({})
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'opencode-agent')
    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(profileTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    const opencodeOption = await screen.findByRole('option', { name: 'OpenCode ACP' })
    await act(async () => {
      fireEvent.click(opencodeOption)
    })
    await user.type(screen.getByLabelText('Model'), 'gpt-5.4')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    expect(toast.error).toHaveBeenCalledWith('OpenCode requires a literal provider/model ID')
    expect(postCount).toBe(0)
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('rejects a blank model for built-in ACP runtimes before posting', async () => {
    useStateModeOverride = 'runtime'
    let postCount = 0
    server.use(http.post('/api/v1/agents', () => {
      postCount += 1
      return HttpResponse.json({})
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'runtime-agent')
    await user.type(screen.getByLabelText('Model'), '   ')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    expect(toast.error).toHaveBeenCalledWith('Model is required for built-in ACP runtimes')
    expect(postCount).toBe(0)
    expect(toast.success).not.toHaveBeenCalled()
    expect(mockNavigate).not.toHaveBeenCalled()
  })

  it('submits an external v2 AgentRuntime reference', async () => {
    useStateModeOverride = 'runtime'
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))

    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'external-agent')
    const profileTrigger = screen.getByText('Runtime profile').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(profileTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    fireEvent.click(await screen.findByRole('option', { name: 'GitHub Copilot ACP' }))
    await waitFor(() => expect(profileTrigger).toHaveTextContent('GitHub Copilot ACP'))
    await user.type(screen.getByLabelText('Model'), 'stale-built-in-model')
    await user.type(screen.getByLabelText('System Prompt'), 'Stale built-in @role')
    await user.type(screen.getByLabelText('Soul (optional)'), 'Stale built-in @persona')
    const sourceTrigger = screen.getByText('Runtime source').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await act(async () => {
      fireEvent.pointerDown(sourceTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    const externalOption = await screen.findByRole('option', { name: 'External v2 AgentRuntime' })
    await act(async () => {
      fireEvent.click(externalOption)
    })
    await waitFor(() => expect(sourceTrigger).toHaveTextContent('External v2 AgentRuntime'))
    await user.type(screen.getByPlaceholderText('external-codex'), 'external-codex')
    expect(screen.queryByLabelText('Model')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('System Prompt')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Soul (optional)')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.runtime).toEqual({ runtimeRef: { name: 'external-codex' } })
    expect(submitted.spec.model).toBeUndefined()
    expect(submitted.spec.systemPrompt).toBeUndefined()
    expect(submitted.spec.soul).toBeUndefined()
  })

  it.each([false, true])('submits native AI agents with a providerRef and role/soul configured: %s', async (withPrompts) => {
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))
    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'native-agent')
    const providerTrigger = screen.getByLabelText('Provider')
    await waitFor(() => expect(providerTrigger).not.toBeDisabled())
    await pickOption(providerTrigger, /anthropic-prod/)
    await waitFor(() => expect(providerTrigger).toHaveTextContent('anthropic-prod'))
    // The model placeholder follows the selected Provider's default model.
    expect(screen.getByLabelText('Model')).toHaveAttribute('placeholder', 'claude-default')
    await user.type(screen.getByLabelText('Model'), 'native-model')
    if (withPrompts) {
      await user.type(screen.getByLabelText('System Prompt'), 'Native AI @role')
      await user.type(screen.getByLabelText('Soul (optional)'), 'Native AI @persona')
    }
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.runtime).toBeUndefined()
    expect(submitted.spec.providerRef).toEqual({ name: 'anthropic-prod' })
    expect(submitted.spec.model).toEqual({ name: 'native-model', temperature: 0.7 })
    expect(submitted.spec.systemPrompt).toEqual(withPrompts ? { inline: 'Native AI @role' } : undefined)
    expect(submitted.spec.soul).toEqual(withPrompts ? { inline: 'Native AI @persona' } : undefined)
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/agents' })
  })

  it('lists namespace Providers and marks the ones that are not ready', async () => {
    render(<AgentCreateForm />)
    const providerTrigger = screen.getByLabelText('Provider')
    await waitFor(() => expect(providerTrigger).not.toBeDisabled())
    await act(async () => {
      fireEvent.pointerDown(providerTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    expect(await screen.findByRole('option', { name: 'anthropic-prod (anthropic)' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'openai-lab (openai) — not ready' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'Anthropic' })).not.toBeInTheDocument()
  })

  it('requires a Provider for native AI agents with an inline error', async () => {
    let postCount = 0
    server.use(http.post('/api/v1/agents', () => {
      postCount += 1
      return HttpResponse.json({})
    }))
    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'native-agent')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    expect(await screen.findByText('Select a Provider for native AI agents.')).toBeInTheDocument()
    expect(screen.getByLabelText('Provider')).toHaveAttribute('aria-invalid', 'true')
    expect(postCount).toBe(0)

    await pickOption(screen.getByLabelText('Provider'), /anthropic-prod/)
    await waitFor(() => expect(screen.queryByText('Select a Provider for native AI agents.')).not.toBeInTheDocument())
  })

  it('links to the Provider docs when the namespace has no Providers', async () => {
    server.use(http.get('/api/v1/providers', () => HttpResponse.json({ items: [], metadata: {} })))
    render(<AgentCreateForm />)
    expect(await screen.findByText(/No Providers in default/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'see the Provider reference' })).toHaveAttribute(
      'href',
      'https://orka-agents.github.io/orka/docs/reference/configuration#provider',
    )
    expect(screen.getByLabelText('Provider')).toBeDisabled()
  })

  it('rejects a non-positive native max tokens value before posting', async () => {
    let postCount = 0
    server.use(http.post('/api/v1/agents', () => {
      postCount += 1
      return HttpResponse.json({})
    }))
    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'native-agent')
    const providerTrigger = screen.getByLabelText('Provider')
    await waitFor(() => expect(providerTrigger).not.toBeDisabled())
    await pickOption(providerTrigger, /anthropic-prod/)
    const maxTokens = screen.getByLabelText('Max Tokens')
    expect(maxTokens).toHaveAttribute('min', '1')
    fireEvent.change(maxTokens, { target: { value: '-5' } })
    fireEvent.submit(screen.getByRole('button', { name: 'Create Agent' }).closest('form')!)

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith('Max tokens must be a positive integer'))
    expect(postCount).toBe(0)
  })

  it('accepts a fractional temperature within bounds', async () => {
    let submitted: any
    server.use(http.post('/api/v1/agents', async ({ request }) => {
      submitted = await request.json()
      return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted.spec })
    }))
    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByPlaceholderText('my-agent'), 'native-agent')
    const providerTrigger = screen.getByLabelText('Provider')
    await waitFor(() => expect(providerTrigger).not.toBeDisabled())
    await pickOption(providerTrigger, /anthropic-prod/)
    const temperature = screen.getByLabelText('Temperature')
    expect(temperature).toHaveAttribute('step', 'any')
    expect(temperature).toHaveAttribute('min', '0')
    expect(temperature).toHaveAttribute('max', '2')
    await user.clear(temperature)
    await user.type(temperature, '0.15')
    await user.click(screen.getByRole('button', { name: 'Create Agent' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Agent created'))
    expect(submitted.spec.model).toEqual({ temperature: 0.15 })
  })

  it('resets mode-specific fields when the mode changes', async () => {
    const user = userEvent.setup()
    render(<AgentCreateForm />)
    await user.type(screen.getByLabelText('Model'), 'native-model')
    await user.type(screen.getByLabelText('Max Tokens'), '2048')

    await pickOption(screen.getByLabelText('Mode'), 'ACP agent runtime')
    await waitFor(() => expect(screen.getByLabelText('Runtime profile')).toBeInTheDocument())
    expect(screen.getByLabelText('Model')).toHaveValue('')

    await pickOption(screen.getByLabelText('Runtime profile'), 'OpenCode ACP')
    await waitFor(() => expect(screen.getByLabelText('Max Output Tokens')).toBeInTheDocument())
    expect(screen.getByLabelText('Max Output Tokens')).toHaveValue(null)
  })

  it('labels the mode and runtime selects for assistive technology', async () => {
    useStateModeOverride = 'runtime'
    render(<AgentCreateForm />)
    expect(screen.getByLabelText('Mode')).toHaveAttribute('role', 'combobox')
    expect(screen.getByLabelText('Runtime source')).toHaveAttribute('role', 'combobox')
    expect(screen.getByLabelText('Runtime profile')).toHaveAttribute('role', 'combobox')
  })
})
