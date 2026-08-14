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

function fill(element: Element, value: string) {
  fireEvent.change(element, { target: { value } })
}

async function openWriteWorkspace(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByText(/Advanced Options/))
  await user.click(screen.getByText(/Workspace policy/))
  const intentTrigger = screen.getByText('Workspace intent').closest('.space-y-2')!.querySelector('[role="combobox"]')!
  fireEvent.pointerDown(intentTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
  fireEvent.click(await screen.findByRole('option', { name: /Write — produce/ }))
}

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

    fill(screen.getByPlaceholderText('my-task'), 'test-task')
    fill(screen.getByPlaceholderText('alpine:latest'), 'nginx:latest')
    fill(screen.getByPlaceholderText('echo hello'), 'ls -la')

    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith('Task created')
    })
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/tasks' })
  })

  it('submits container task without command', async () => {
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    fill(screen.getByPlaceholderText('my-task'), 'no-cmd-task')
    fill(screen.getByPlaceholderText('alpine:latest'), 'nginx:latest')
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

    fill(screen.getByPlaceholderText('my-task'), 'bad-task')
    fill(screen.getByPlaceholderText('alpine:latest'), 'nginx')

    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalled()
    })
  })

  it('submits AI task form and navigates', async () => {
    useStateTypeOverride = 'ai'
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    fill(screen.getByPlaceholderText('my-task'), 'ai-task')
    fill(screen.getByPlaceholderText('claude-sonnet-4-20250514'), 'my-model')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'Hello AI')

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

    fill(screen.getByPlaceholderText('my-task'), 'agent-task')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'Do something')

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

  it('shows role-specific workspace credential names and optional keys', async () => {
    useStateTypeOverride = 'agent'
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            { metadata: { name: 'my-agent', namespace: 'default' }, spec: { runtime: { type: 'codex' } } },
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.click(screen.getByText(/Advanced Options/))
    expect(screen.queryByText('Max Turns')).not.toBeInTheDocument()
    expect(screen.queryByText('Allow Bash')).not.toBeInTheDocument()

    await user.click(screen.getByText(/Workspace policy/))
    expect(screen.getByText('Workspace intent')).toBeInTheDocument()
    expect(screen.getAllByText(/Read — verified workspace must remain unchanged/).length).toBeGreaterThan(0)
    expect(screen.getByText('Source repository URL')).toBeInTheDocument()
    expect(screen.getByLabelText('Source repository URL')).not.toBeRequired()
    expect(screen.getByLabelText('Source repository URL identity')).toHaveAttribute('placeholder', 'github.com/org/repo')
    expect(screen.queryByPlaceholderText('R_kgDOExample')).not.toBeInTheDocument()
    expect(screen.getByText(/normalized credential-free URL identity/)).toBeInTheDocument()
    expect(screen.getByLabelText('Read credential Secret')).toBeInTheDocument()
    expect(screen.getByLabelText('Read credential key')).toHaveAttribute('placeholder', 'token (default)')
    expect(screen.queryByLabelText('Publication write credential Secret')).not.toBeInTheDocument()

    const intentTrigger = screen.getByText('Workspace intent').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    fireEvent.pointerDown(intentTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    await waitFor(() => expect(screen.getByRole('option', { name: /Write — produce/ })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('option', { name: /Write — produce/ }))

    for (const label of [
      'Publication read credential Secret',
      'Publication read credential key',
      'Publication write credential Secret',
      'Publication write credential key',
      'Forge credential Secret',
      'Forge credential key',
    ]) {
      expect(await screen.findByLabelText(label)).toBeInTheDocument()
    }
    expect(screen.getByLabelText('Source repository URL')).toBeRequired()
    expect(screen.getByLabelText('Publication write credential Secret')).toBeRequired()
    expect(screen.getByLabelText('Pull request base branch')).not.toBeRequired()
    expect(screen.getByLabelText('Forge credential Secret')).not.toBeRequired()
    expect(screen.getByText('Publication repository URL')).toBeInTheDocument()
    expect(screen.getByLabelText('Publication repository URL identity')).toHaveAttribute('placeholder', 'github.com/org/repo')
    expect(screen.getAllByText(/normalized credential-free URL identity/)).toHaveLength(2)
    expect(screen.getByText('Publication branch')).toBeInTheDocument()
    expect(screen.getByText(/Secret values are never shown/)).toBeInTheDocument()
    expect(screen.getByText(/Reconcile a pull request/)).toBeInTheDocument()
    await user.click(screen.getByRole('switch', { name: /Reconcile a pull request/ }))
    expect(screen.getByLabelText('Pull request base branch')).toBeRequired()
    expect(screen.getByLabelText('Forge credential Secret')).toBeRequired()
  })

  it('submits top-level write workspace with distinct credential roles and keys', async () => {
    useStateTypeOverride = 'agent'
    let submitted: any
    server.use(
      http.get('/api/v1/agents', () => HttpResponse.json({ items: [] })),
      http.post('/api/v1/tasks', async ({ request }) => {
        submitted = await request.json()
        return HttpResponse.json({ metadata: { name: submitted.name }, spec: submitted })
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    fill(screen.getByPlaceholderText('my-task'), 'write-task')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'Update the repository')
    await user.click(screen.getByText(/Advanced Options/))
    await user.click(screen.getByText(/Workspace policy/))

    const intentTrigger = screen.getByText('Workspace intent').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    fireEvent.pointerDown(intentTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    fireEvent.click(await screen.findByRole('option', { name: /Write — produce/ }))

    const repositoryURLs = screen.getAllByPlaceholderText('https://github.com/org/repo')
    fill(repositoryURLs[0], 'https://github.com/source/repo')
    fill(screen.getByLabelText('Source repository provider'), 'github')
    fill(screen.getByLabelText('Source repository URL identity'), 'github.com/source/repo')
    fill(screen.getByLabelText('Read credential Secret'), 'source-read')
    fill(screen.getByLabelText('Read credential key'), 'source-token')
    fill(repositoryURLs[1], 'https://github.com/publish/repo')
    fill(screen.getByLabelText('Publication provider'), 'github')
    fill(screen.getByLabelText('Publication repository URL identity'), 'github.com/publish/repo')
    fill(screen.getByLabelText('Publication read credential Secret'), 'target-read')
    fill(screen.getByLabelText('Publication read credential key'), 'verify-token')
    fill(screen.getByLabelText('Publication write credential Secret'), 'target-write')
    fill(screen.getByLabelText('Publication write credential key'), 'write-token')
    fill(screen.getByLabelText('Forge credential Secret'), 'forge-api')
    fill(screen.getByLabelText('Forge credential key'), 'forge-token')
    fill(screen.getByPlaceholderText('Leave empty for an Orka-owned branch'), 'orka/change')
    fill(screen.getByLabelText('Pull request base branch'), 'main')
    await user.click(screen.getByRole('switch', { name: /Reconcile a pull request/ }))
    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith('Task created'))
    expect(submitted.workspace).toEqual({
      intent: 'write',
      gitRepo: 'https://github.com/source/repo',
      sourceRepository: { provider: 'github', id: 'github.com/source/repo' },
      readCredentialRef: { name: 'source-read', key: 'source-token' },
      publicationGitRepo: 'https://github.com/publish/repo',
      publicationRepository: { provider: 'github', id: 'github.com/publish/repo' },
      publicationReadCredentialRef: { name: 'target-read', key: 'verify-token' },
      publicationCredentialRef: { name: 'target-write', key: 'write-token' },
      forgeCredentialRef: { name: 'forge-api', key: 'forge-token' },
      pushBranch: 'orka/change',
      prBaseBranch: 'main',
      createPR: true,
    })
    expect(JSON.stringify(submitted)).not.toContain('must-never-render')
    expect(submitted.agentRuntime?.workspace).toBeUndefined()
    expect(submitted.workspace.gitSecretRef).toBeUndefined()
    expect(submitted.workspace.forkRepo).toBeUndefined()
  })

  it('requires a source repository URL for write workspaces', async () => {
    useStateTypeOverride = 'agent'
    let postCount = 0
    server.use(
      http.get('/api/v1/agents', () => HttpResponse.json({ items: [] })),
      http.post('/api/v1/tasks', () => {
        postCount += 1
        return HttpResponse.json({})
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    fill(screen.getByPlaceholderText('my-task'), 'write-task')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'Update the repository')
    await openWriteWorkspace(user)
    fill(screen.getByLabelText('Source repository URL'), '   ')
    fill(screen.getByLabelText('Publication write credential Secret'), 'target-write')
    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    expect(toast.error).toHaveBeenCalledWith('Source repository URL is required for write workspaces')
    expect(postCount).toBe(0)
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('requires a publication write credential for write workspaces', async () => {
    useStateTypeOverride = 'agent'
    let postCount = 0
    server.use(
      http.get('/api/v1/agents', () => HttpResponse.json({ items: [] })),
      http.post('/api/v1/tasks', () => {
        postCount += 1
        return HttpResponse.json({})
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    fill(screen.getByPlaceholderText('my-task'), 'write-task')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'Update the repository')
    await openWriteWorkspace(user)
    fill(screen.getByLabelText('Source repository URL'), 'https://github.com/source/repo')
    fill(screen.getByLabelText('Publication write credential Secret'), '   ')
    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    expect(toast.error).toHaveBeenCalledWith('Publication write credential Secret is required for write workspaces')
    expect(postCount).toBe(0)
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('requires a pull request base branch when creating a pull request', async () => {
    useStateTypeOverride = 'agent'
    let postCount = 0
    server.use(
      http.get('/api/v1/agents', () => HttpResponse.json({ items: [] })),
      http.post('/api/v1/tasks', () => {
        postCount += 1
        return HttpResponse.json({})
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    fill(screen.getByPlaceholderText('my-task'), 'write-task')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'Update the repository')
    await openWriteWorkspace(user)
    fill(screen.getByLabelText('Source repository URL'), 'https://github.com/source/repo')
    fill(screen.getByLabelText('Publication write credential Secret'), 'target-write')
    fill(screen.getByLabelText('Forge credential Secret'), 'forge-api')
    fill(screen.getByLabelText('Pull request base branch'), '   ')
    await user.click(screen.getByRole('switch', { name: /Reconcile a pull request/ }))
    await user.click(screen.getByRole('button', { name: 'Create Task' }))

    expect(toast.error).toHaveBeenCalledWith('Pull request base branch is required when creating a pull request')
    expect(postCount).toBe(0)
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('requires a forge credential before creating a pull request', async () => {
    useStateTypeOverride = 'agent'
    let postCount = 0
    server.use(
      http.get('/api/v1/agents', () => HttpResponse.json({ items: [] })),
      http.post('/api/v1/tasks', () => {
        postCount += 1
        return HttpResponse.json({})
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    fill(screen.getByPlaceholderText('my-task'), 'write-task')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'Update the repository')
    await openWriteWorkspace(user)
    fill(screen.getByLabelText('Source repository URL'), 'https://github.com/source/repo')
    fill(screen.getByLabelText('Publication write credential Secret'), 'target-write')
    fill(screen.getByLabelText('Pull request base branch'), 'main')
    await user.click(screen.getByRole('switch', { name: /Reconcile a pull request/ }))
    const submitButton = screen.getByRole('button', { name: 'Create Task' })
    fireEvent.submit(submitButton.closest('form')!)

    expect(toast.error).toHaveBeenCalledWith('Forge credential Secret is required when creating a pull request')
    expect(postCount).toBe(0)
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('does not show workspace config for non-agent types', async () => {
    const user = userEvent.setup()
    render(<TaskCreateForm />)

    await user.click(screen.getByText(/Advanced Options/))

    expect(screen.queryByText('Workspace policy')).not.toBeInTheDocument()
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
                runtime: { type: 'codex' },
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
    const trigger = screen.getByText('Agent Reference').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    await waitFor(() => expect(trigger).not.toBeDisabled())
    fireEvent.pointerDown(trigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    await waitFor(() => {
      expect(screen.getByRole('option', { name: /coord-agent/ })).toBeInTheDocument()
    })
    fireEvent.click(screen.getByRole('option', { name: /coord-agent/ }))

    await waitFor(() => {
      expect(screen.getByTestId('agent-info-card')).toBeInTheDocument()
    })
    expect(screen.getByText('anthropic')).toBeInTheDocument()
    expect(screen.getByText('claude-sonnet')).toBeInTheDocument()
    expect(screen.getByText('codex ACP')).toBeInTheDocument()
    expect(screen.getByText('Coordination')).toBeInTheDocument()
    expect(screen.getByText('2 tools')).toBeInTheDocument()
  })

  it('hides external-runtime agents that cannot be dispatched', async () => {
    useStateTypeOverride = 'agent'
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'built-in-agent', namespace: 'default' },
              spec: { runtime: { type: 'codex' } },
            },
            {
              metadata: { name: 'external-agent', namespace: 'default' },
              spec: { runtime: { runtimeRef: { name: 'external-codex' } } },
            },
            {
              metadata: { name: 'provider-agent', namespace: 'default' },
              spec: { model: { provider: 'openai', name: 'gpt-5.4' } },
            },
          ],
        }),
      ),
    )
    render(<TaskCreateForm />)

    expect(await screen.findByText(/Agents without a built-in CLI runtime are hidden/)).toBeInTheDocument()
    const trigger = screen.getByText('Agent Reference').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    fireEvent.pointerDown(trigger, { button: 0, pointerId: 1, pointerType: 'mouse' })

    expect(await screen.findByRole('option', { name: /built-in-agent/ })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /external-agent/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /provider-agent/ })).not.toBeInTheDocument()
  })
})

describe('TaskCreateForm parity extensions', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
    mockNavigate.mockClear()
  })

  it('submits schedule, env, retry, webhook, and session fields', async () => {
    let posted: any
    server.use(
      http.post('/api/v1/tasks', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: 'cron-task', namespace: 'default' }, spec: { type: 'container' } }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)
    fill(screen.getByPlaceholderText('my-task'), 'cron-task')
    fill(screen.getByPlaceholderText('alpine:latest'), 'alpine:3')
    await user.click(screen.getByText(/Advanced Options/))
    fill(screen.getByLabelText('Cron'), '0 9 * * 1-5')
    fill(screen.getByLabelText('Time zone'), 'UTC')
    fill(screen.getByLabelText(/Environment/), 'LOG_LEVEL=debug\nREGION=us-west-2')
    fill(screen.getByLabelText('Webhook URL'), 'https://hooks.example.com/x')
    fill(screen.getByLabelText('Secret ref'), 'creds')
    fill(screen.getByLabelText('Max retries'), '2')
    fill(screen.getByLabelText('Session name'), 'sess-1')
    await user.click(screen.getByRole('button', { name: 'Create Task' }))
    await waitFor(() => expect(posted).toBeTruthy())
    expect(posted.schedule).toBe('0 9 * * 1-5')
    expect(posted.timeZone).toBe('UTC')
    expect(posted.env).toEqual([
      { name: 'LOG_LEVEL', value: 'debug' },
      { name: 'REGION', value: 'us-west-2' },
    ])
    expect(posted.webhookURL).toBe('https://hooks.example.com/x')
    expect(posted.secretRef).toEqual({ name: 'creds' })
    expect(posted.retryPolicy).toEqual({ maxRetries: 2 })
    expect(posted.sessionRef).toEqual({ name: 'sess-1' })
  })

  it('submits AI extras: system prompt, sampling, tools, and skills', async () => {
    useStateTypeOverride = 'ai'
    let posted: any
    server.use(
      http.post('/api/v1/tasks', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: 'ai-task', namespace: 'default' }, spec: { type: 'ai' } }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)
    fill(screen.getByPlaceholderText('my-task'), 'ai-task')
    fill(screen.getByPlaceholderText('claude-sonnet-4-20250514'), 'claude-sonnet-4')
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'summarize the repo')
    fill(screen.getByLabelText(/System prompt/), 'be terse')
    fill(screen.getByLabelText(/Temperature/), '0.3')
    fill(screen.getByLabelText(/Tools \(comma-separated/), 'web_search, code_exec')
    fill(screen.getByLabelText(/Skills \(comma-separated/), 'code-review')
    await user.click(screen.getByRole('button', { name: 'Create Task' }))
    await waitFor(() => expect(posted).toBeTruthy())
    expect(posted.ai.systemPrompt).toBe('be terse')
    expect(posted.ai.temperature).toBeCloseTo(0.3)
    expect(posted.ai.tools).toEqual(['web_search', 'code_exec'])
    expect(posted.ai.skills).toEqual([{ name: 'code-review' }])
  })

  it('submits agent runtime overrides', async () => {
    useStateTypeOverride = 'agent'
    let posted: any
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json({
          items: [
            {
              metadata: { name: 'coder', namespace: 'default' },
              spec: { runtime: { type: 'claude', contractVersion: 'orka.harness.v2' } },
            },
          ],
          metadata: {},
        }),
      ),
      http.post('/api/v1/tasks', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: 'agent-task', namespace: 'default' }, spec: { type: 'agent' } }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)
    fill(screen.getByPlaceholderText('my-task'), 'agent-task')
    const agentTrigger = screen.getByText('Agent Reference').closest('.space-y-2')!.querySelector('[role="combobox"]')!
    // The select stays disabled until the agents query resolves.
    await waitFor(() => expect(agentTrigger).not.toBeDisabled())
    fireEvent.pointerDown(agentTrigger, { button: 0, pointerId: 1, pointerType: 'mouse' })
    fireEvent.click(await screen.findByRole('option', { name: /coder/ }))
    fill(screen.getByPlaceholderText('Enter your prompt...'), 'fix the bug')
    await user.click(screen.getByText(/Advanced Options/))
    fill(screen.getByLabelText('Max turns'), '25')
    fill(screen.getByLabelText('Allowed tools'), 'Read, Grep')
    await user.click(screen.getByRole('switch', { name: 'Allow bash' }))
    await user.click(screen.getByRole('button', { name: 'Create Task' }))
    await waitFor(() => expect(posted).toBeTruthy())
    expect(posted.agentRuntime).toEqual({ maxTurns: 25, allowedTools: ['Read', 'Grep'], allowBash: true })
  })

  it('opens the YAML editor seeded from form state and submits it', async () => {
    let posted: any
    server.use(
      http.post('/api/v1/tasks', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: 'yaml-task', namespace: 'default' }, spec: { type: 'container' } }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<TaskCreateForm />)
    fill(screen.getByPlaceholderText('my-task'), 'yaml-task')
    fill(screen.getByPlaceholderText('alpine:latest'), 'alpine:3')
    await user.click(screen.getByRole('button', { name: /edit as yaml/i }))
    const textarea = await screen.findByLabelText('Manifest YAML')
    expect((textarea as HTMLTextAreaElement).value).toContain('yaml-task')
    expect((textarea as HTMLTextAreaElement).value).toContain('alpine:3')
    await user.click(screen.getByRole('button', { name: /create task$/i }))
    await waitFor(() => expect(posted).toBeTruthy())
    expect(posted.name).toBe('yaml-task')
    expect(posted.image).toBe('alpine:3')
  })
})
