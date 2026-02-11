import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'

let useStateTypeOverride: string | null = null

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('react', async () => {
  const actual = await vi.importActual('react')
  return {
    ...actual,
    useState: (initial: any) => {
      if (initial === 'container' && useStateTypeOverride) {
        const override = useStateTypeOverride
        useStateTypeOverride = null
        return (actual as any).useState(override)
      }
      return (actual as any).useState(initial)
    },
  }
})

const mockNavigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => mockNavigate,
    useLocation: () => ({ pathname: '/tasks/new' }),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { toast } from 'sonner'
import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { TaskCreateForm } from './task-create-form'

describe('TaskCreateForm', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    useStateTypeOverride = null
    mockNavigate.mockClear()
    vi.mocked(toast.success).mockClear()
    vi.mocked(toast.error).mockClear()
    // Polyfill pointer capture methods missing in jsdom (needed by Radix Select)
    if (!Element.prototype.hasPointerCapture) {
      Element.prototype.hasPointerCapture = () => false
    }
    if (!Element.prototype.setPointerCapture) {
      Element.prototype.setPointerCapture = () => {}
    }
    if (!Element.prototype.releasePointerCapture) {
      Element.prototype.releasePointerCapture = () => {}
    }
    if (!Element.prototype.scrollIntoView) {
      Element.prototype.scrollIntoView = () => {}
    }
    if (!globalThis.ResizeObserver) {
      globalThis.ResizeObserver = class {
        observe() {}
        unobserve() {}
        disconnect() {}
      } as unknown as typeof ResizeObserver
    }
  })

  it('renders form with name and type fields', () => {
    render(<TaskCreateForm />)
    expect(screen.getByText('Name')).toBeInTheDocument()
    expect(screen.getByText('Type')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('my-task')).toBeInTheDocument()
  })

  it('container type shows image and command inputs', () => {
    render(<TaskCreateForm />)
    // Container is default type
    expect(screen.getByText('Image')).toBeInTheDocument()
    expect(screen.getByText('Command')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('alpine:latest')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('echo hello')).toBeInTheDocument()
  })

  it('AI type shows provider, model, prompt fields', async () => {
    render(<TaskCreateForm />)

    // Open the type select
    const typeTrigger = screen.getByText('Type').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    fireEvent.pointerDown(typeTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    await waitFor(() => {
      expect(screen.getByRole('option', { name: 'AI' })).toBeInTheDocument()
    })
    fireEvent.click(screen.getByRole('option', { name: 'AI' }))

    await waitFor(() => {
      expect(screen.getByText('Provider')).toBeInTheDocument()
    })
    expect(screen.getByText('Model')).toBeInTheDocument()
    expect(screen.getByText('Prompt')).toBeInTheDocument()
  })

  it('Agent type shows agent reference and prompt fields', async () => {
    render(<TaskCreateForm />)

    const typeTrigger = screen.getByText('Type').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    fireEvent.pointerDown(typeTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    await waitFor(() => {
      expect(screen.getByRole('option', { name: 'Agent' })).toBeInTheDocument()
    })
    fireEvent.click(screen.getByRole('option', { name: 'Agent' }))

    await waitFor(() => {
      expect(screen.getByText('Agent Reference')).toBeInTheDocument()
    })
    expect(screen.getByText('Prompt')).toBeInTheDocument()
  })

  it('renders Create Task and Cancel buttons', () => {
    render(<TaskCreateForm />)
    expect(screen.getByRole('button', { name: 'Create Task' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument()
  })

  it('submits container task and navigates', async () => {
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.type(screen.getByPlaceholderText('my-task'), 'test-task')
    await user.type(screen.getByPlaceholderText('alpine:latest'), 'nginx:latest')
    await user.type(screen.getByPlaceholderText('echo hello'), 'ls -la')

    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Task created')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/tasks' })
  })

  it('submits container task without command', async () => {
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.type(screen.getByPlaceholderText('my-task'), 'no-cmd-task')
    await user.type(screen.getByPlaceholderText('alpine:latest'), 'nginx:latest')
    // Don't fill in command to test the `if (command)` branch false path

    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Task created')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/tasks' })
  })

  it('cancel button navigates to tasks', async () => {
    const user = userEvent.setup()
    render(<TaskCreateForm />)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/tasks' })
  })

  it('shows error toast when submission fails', async () => {
    server.use(
      http.post('/api/v1/tasks', () => new HttpResponse('Bad request', { status: 400 })),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.type(screen.getByPlaceholderText('my-task'), 'bad-task')
    await user.type(screen.getByPlaceholderText('alpine:latest'), 'nginx')

    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalled()
    })
  })

  it('submits AI task form and navigates', async () => {
    useStateTypeOverride = 'ai'
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.type(screen.getByPlaceholderText('my-task'), 'ai-task')
    await user.type(screen.getByPlaceholderText('claude-sonnet-4-20250514'), 'my-model')
    await user.type(screen.getByPlaceholderText('Enter your prompt...'), 'Hello AI')

    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Task created')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/tasks' })
  })

  it('submits Agent task form and navigates', async () => {
    useStateTypeOverride = 'agent'
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            { metadata: { name: 'my-agent', namespace: 'default' }, spec: { model: { name: 'claude' } } },
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.type(screen.getByPlaceholderText('my-task'), 'agent-task')
    await user.type(screen.getByPlaceholderText('Enter your prompt...'), 'Do something')

    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Task created')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/tasks' })
  })

  it('toggles advanced options visibility', async () => {
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    expect(screen.queryByText('Priority')).not.toBeInTheDocument()
    expect(screen.queryByText('Timeout')).not.toBeInTheDocument()

    await user.click(screen.getByText(/Advanced Options/))

    expect(screen.getByText('Priority')).toBeInTheDocument()
    expect(screen.getByText('Timeout')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('500')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('30m')).toBeInTheDocument()
  })

  it('shows workspace config fields when agent type is selected and advanced expanded', async () => {
    useStateTypeOverride = 'agent'
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            { metadata: { name: 'my-agent', namespace: 'default' }, spec: { model: { name: 'claude' } } },
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.click(screen.getByText(/Advanced Options/))

    expect(screen.getByText('Max Turns')).toBeInTheDocument()
    expect(screen.getByText('Allow Bash')).toBeInTheDocument()

    await user.click(screen.getByText(/Workspace Configuration/))

    expect(screen.getByText('Git Repo URL')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('https://github.com/org/repo')).toBeInTheDocument()
    expect(screen.getByText('Branch')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('main')).toBeInTheDocument()
    expect(screen.getByText('Push Branch')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('feature/my-task')).toBeInTheDocument()
    expect(screen.getByText('Git Secret Ref')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('git-credentials')).toBeInTheDocument()
  })

  it('does not show workspace config for non-agent types', async () => {
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.click(screen.getByText(/Advanced Options/))

    expect(screen.queryByText('Max Turns')).not.toBeInTheDocument()
    expect(screen.queryByText('Workspace Configuration')).not.toBeInTheDocument()
  })

  it('shows agent info card when agent is selected', async () => {
    useStateTypeOverride = 'agent'
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'coord-agent', namespace: 'default' },
              spec: {
                model: { provider: 'anthropic', name: 'claude-sonnet' },
                runtime: { type: 'copilot' },
                coordination: { enabled: true },
                tools: [{ name: 'tool1' }, { name: 'tool2' }],
              },
            },
          ],
        }),
      ),
    )
    render(<TaskCreateForm />)

    // Wait for agents to load and select the agent
    await waitFor(() => {
      const trigger = screen.getByText('Agent Reference').closest('.space-y-2')!.querySelector('[role="combobox"]')!
      fireEvent.pointerDown(trigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    })
    await waitFor(() => {
      expect(screen.getByRole('option', { name: /coord-agent/ })).toBeInTheDocument()
    })
    fireEvent.click(screen.getByRole('option', { name: /coord-agent/ }))

    await waitFor(() => {
      expect(screen.getByTestId('agent-info-card')).toBeInTheDocument()
    })
    expect(screen.getByText('anthropic')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet')).toBeInTheDocument()
    expect(screen.getByText('copilot runtime')).toBeInTheDocument()
    expect(screen.getByText('Coordination')).toBeInTheDocument()
    expect(screen.getByText('2 tools')).toBeInTheDocument()
  })
})
