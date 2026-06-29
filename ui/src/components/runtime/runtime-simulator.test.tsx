import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to }: any) => <a href={to}>{children}</a>, useNavigate: () => vi.fn() }
})

import { RuntimeSimulator } from './runtime-simulator'

describe('RuntimeSimulator', () => {
  it('is labeled a simulator and steps via local fixtures only', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    render(<RuntimeSimulator />)
    expect(screen.getByText('SIMULATOR')).toBeInTheDocument()
    expect(screen.getByText(/no real tasks/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /step/i }))
    await userEvent.click(screen.getByRole('button', { name: /inject failure/i }))
    await userEvent.click(screen.getByRole('button', { name: /reset/i }))
    // Simulator must never hit a production mutating API.
    expect(fetchSpy).not.toHaveBeenCalled()
    fetchSpy.mockRestore()
  })
})
