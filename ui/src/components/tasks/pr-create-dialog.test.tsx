import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useUIStore } from '@/stores/ui'
import { PRCreateDialog } from './pr-create-dialog'

describe('PRCreateDialog', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default' })
  })

  it('returns null when no pushBranch', () => {
    const { container } = render(<PRCreateDialog taskName="test" />)
    expect(container.innerHTML).toBe('')
  })

  it('shows Create PR button when pushBranch set', () => {
    render(<PRCreateDialog taskName="test" pushBranch="feature/x" />)
    expect(screen.getByText('Create PR')).toBeInTheDocument()
  })

  it('opens dialog on click', async () => {
    const user = userEvent.setup()
    render(<PRCreateDialog taskName="test" pushBranch="feature/x" summary="Test summary" />)
    await user.click(screen.getByText('Create PR'))
    expect(screen.getByRole('heading', { name: 'Create Pull Request' })).toBeInTheDocument()
    expect(screen.getByDisplayValue('Changes from task test')).toBeInTheDocument()
    expect(screen.getByDisplayValue('Test summary')).toBeInTheDocument()
  })
})
