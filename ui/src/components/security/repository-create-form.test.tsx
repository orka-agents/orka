import { describe, it, expect, beforeEach, vi } from 'vitest'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const mockNavigate = vi.fn()
const mockUseAgentList = vi.fn()

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    useNavigate: () => mockNavigate,
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

vi.mock('@/hooks/use-security', () => ({
  useCreateRepositoryScan: () => ({ mutateAsync: vi.fn(), isPending: false }),
}))

vi.mock('@/hooks/use-secrets', () => ({
  useSecretNames: () => ({ data: { items: [] } }),
}))

vi.mock('@/hooks/use-agents', () => ({
  useAgentList: (...args: unknown[]) => mockUseAgentList(...args),
}))

if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as any
}

import userEvent from '@testing-library/user-event'
import { render, screen } from '@/test/test-utils'
import { useUIStore } from '@/stores/ui'
import { RepositoryCreateForm } from './repository-create-form'

describe('RepositoryCreateForm', () => {
  beforeEach(() => {
    mockNavigate.mockReset()
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    mockUseAgentList.mockImplementation((options?: { namespace?: string; enabled?: boolean }) => {
      if (options?.enabled === false) {
        return { data: undefined, isLoading: false }
      }

      const namespace = options?.namespace ?? useUIStore.getState().namespace
      if (namespace === 'orka-system') {
        return {
          data: { items: [{ metadata: { name: 'gpt54-assistant' } }] },
          isLoading: false,
        }
      }

      return {
        data: { items: [] },
        isLoading: false,
      }
    })
  })

  it('shows a namespace hint and lets the user switch to orka-system when agents only exist there', async () => {
    const user = userEvent.setup()
    render(<RepositoryCreateForm />)

    expect(screen.getByText(/Showing agents from the/i)).toBeInTheDocument()
    expect(screen.getByText(/No agents are available in this namespace/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Register Repository' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: 'Switch to orka-system' }))

    expect(useUIStore.getState().namespace).toBe('orka-system')
  })
})
