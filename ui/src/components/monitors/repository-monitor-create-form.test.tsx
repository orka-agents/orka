import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockNavigate = vi.fn()
const mockMutateAsync = vi.fn()
const mockUseCreateRepositoryMonitor = vi.fn()
const mockUseAgentList = vi.fn()
const mockUseSecretNames = vi.fn()
const mockToastSuccess = vi.fn()
const mockToastError = vi.fn()

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    useNavigate: () => mockNavigate,
  }
})

vi.mock('sonner', () => ({
  toast: {
    success: (...args: unknown[]) => mockToastSuccess(...args),
    error: (...args: unknown[]) => mockToastError(...args),
  },
}))

vi.mock('@/hooks/use-monitors', () => ({
  useCreateRepositoryMonitor: () => mockUseCreateRepositoryMonitor(),
}))

vi.mock('@/hooks/use-agents', () => ({
  useAgentList: () => mockUseAgentList(),
}))

vi.mock('@/hooks/use-secrets', () => ({
  useSecretNames: () => mockUseSecretNames(),
}))


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

import userEvent from '@testing-library/user-event'
import { render, screen, waitFor } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { RepositoryMonitorCreateForm } from './repository-monitor-create-form'

describe('RepositoryMonitorCreateForm', () => {
  beforeEach(() => {
    mockNavigate.mockReset()
    mockMutateAsync.mockReset()
    mockToastSuccess.mockReset()
    mockToastError.mockReset()
    mockUseCreateRepositoryMonitor.mockReturnValue({ mutateAsync: mockMutateAsync, isPending: false })
    mockUseAgentList.mockReturnValue({
      data: {
        items: [
          { metadata: { name: 'repo-reviewer' }, spec: { runtime: { type: 'claude' } } },
          { metadata: { name: 'codex-agent' }, spec: { runtime: { type: 'codex' } } },
        ],
      },
      isLoading: false,
    })
    mockUseSecretNames.mockReturnValue({ data: { items: [{ name: 'repo-monitor-github' }] } })
    mockMutateAsync.mockResolvedValue({
      metadata: { name: 'example-app', namespace: 'default' },
      spec: { repoURL: 'https://github.com/example/app' },
    })
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('submits the repository monitor create body and navigates to the created monitor', async () => {
    const user = userEvent.setup()
    render(<RepositoryMonitorCreateForm />)

    await user.type(screen.getByLabelText(/Monitor name/i), 'example-app')
    await user.type(screen.getByLabelText(/GitHub repository URL/i), 'https://github.com/example/app')
    await user.clear(screen.getByLabelText(/Branch/i))
    await user.type(screen.getByLabelText(/Branch/i), 'develop')
    await user.type(screen.getByLabelText(/Reviewer Agent name/i), 'repo-reviewer')
    await user.type(screen.getByLabelText(/Git Secret name/i), 'repo-monitor-github')
    await user.clear(screen.getByLabelText(/Schedule/i))
    await user.type(screen.getByLabelText(/Schedule/i), '*/15 * * * *')
    await user.clear(screen.getByLabelText(/Max PRs per run/i))
    await user.type(screen.getByLabelText(/Max PRs per run/i), '10')
    await user.click(screen.getByRole('checkbox', { name: /Include draft pull requests/i }))
    await user.click(screen.getByRole('checkbox', { name: /Enable exact event runs/i }))
    await user.click(screen.getByRole('combobox', { name: /Review event/i }))
    await user.click(await screen.findByRole('option', { name: 'REQUEST_CHANGES' }))
    await user.type(screen.getByLabelText(/Stale review TTL/i), '24h')
    await user.type(screen.getByLabelText(/Protected labels/i), 'security-sensitive, customer-data, security-sensitive')
    await user.type(screen.getByLabelText(/Pause labels/i), 'orka:pause')

    await user.click(screen.getByRole('button', { name: /Create Monitor/i }))

    await waitFor(() => expect(mockMutateAsync).toHaveBeenCalledTimes(1))
    expect(mockMutateAsync).toHaveBeenCalledWith({
      name: 'example-app',
      namespace: 'default',
      spec: {
        provider: 'github',
        repoURL: 'https://github.com/example/app',
        branch: 'develop',
        schedule: '*/15 * * * *',
        gitSecretRef: { name: 'repo-monitor-github' },
        targets: {
          pullRequests: {
            enabled: true,
            includeDrafts: true,
            maxPerRun: 10,
          },
        },
        agents: {
          reviewer: { name: 'repo-reviewer' },
        },
        review: {
          event: 'REQUEST_CHANGES',
          staleReviewTTL: '24h',
          exactEventEnabled: true,
        },
        policy: {
          protectedLabels: ['security-sensitive', 'customer-data'],
          pauseLabels: ['orka:pause'],
        },
      },
    })
    expect(mockToastSuccess).toHaveBeenCalledWith('Repository monitor created')
    expect(mockNavigate).toHaveBeenCalledWith({ to: '/monitors/$monitorId', params: { monitorId: 'example-app' } })
  })

  it('rejects non-root GitHub pull request URLs before submitting', async () => {
    const user = userEvent.setup()
    render(<RepositoryMonitorCreateForm />)

    await user.type(screen.getByLabelText(/Monitor name/i), 'example-app')
    await user.type(screen.getByLabelText(/GitHub repository URL/i), 'https://github.com/example/app/pull/1')
    await user.type(screen.getByLabelText(/Reviewer Agent name/i), 'repo-reviewer')

    await user.click(screen.getByRole('button', { name: /Create Monitor/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Repository URL must be a credential-free GitHub repository root')
    expect(mockToastError).toHaveBeenCalledWith(expect.stringContaining('Repository URL must be a credential-free GitHub repository root'))
    expect(mockMutateAsync).not.toHaveBeenCalled()
  })

  it('requires a reviewer Agent name', async () => {
    const user = userEvent.setup()
    render(<RepositoryMonitorCreateForm />)

    await user.type(screen.getByLabelText(/Monitor name/i), 'example-app')
    await user.type(screen.getByLabelText(/GitHub repository URL/i), 'https://github.com/example/app')

    await user.click(screen.getByRole('button', { name: /Create Monitor/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Reviewer Agent name is required')
    expect(mockMutateAsync).not.toHaveBeenCalled()
  })
})
