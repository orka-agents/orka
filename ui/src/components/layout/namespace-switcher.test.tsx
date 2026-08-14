import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { NamespaceSwitcher } from './namespace-switcher'

describe('NamespaceSwitcher', () => {
  beforeEach(() => {
    useUIStore.setState({
      sidebarCollapsed: false,
      theme: 'light',
      namespace: 'default',
      namespaceHistory: ['team-a'],
    })
  })

  it('shows the current namespace on the trigger', () => {
    render(<NamespaceSwitcher />)
    expect(screen.getByRole('combobox', { name: /switch namespace/i })).toHaveTextContent('default')
  })

  it('applies a typed namespace on submit', async () => {
    const user = userEvent.setup()
    render(<NamespaceSwitcher />)
    await user.click(screen.getByRole('combobox', { name: /switch namespace/i }))
    const input = await screen.findByLabelText('Namespace')
    await user.type(input, 'prod-agents{Enter}')
    expect(useUIStore.getState().namespace).toBe('prod-agents')
    expect(useUIStore.getState().namespaceHistory[0]).toBe('prod-agents')
  })

  it('lists history and defaults as suggestions and applies on click', async () => {
    const user = userEvent.setup()
    render(<NamespaceSwitcher />)
    await user.click(screen.getByRole('combobox', { name: /switch namespace/i }))
    const list = await screen.findByRole('list', { name: /namespace suggestions/i })
    expect(list).toHaveTextContent('team-a')
    expect(list).toHaveTextContent('orka-system')
    await user.click(screen.getByRole('button', { name: /team-a/i }))
    await waitFor(() => expect(useUIStore.getState().namespace).toBe('team-a'))
  })

  it('filters suggestions by the draft text', async () => {
    const user = userEvent.setup()
    render(<NamespaceSwitcher />)
    await user.click(screen.getByRole('combobox', { name: /switch namespace/i }))
    const input = await screen.findByLabelText('Namespace')
    await user.type(input, 'orka')
    const list = screen.getByRole('list', { name: /namespace suggestions/i })
    expect(list).toHaveTextContent('orka-system')
    expect(list).not.toHaveTextContent('team-a')
  })
})
