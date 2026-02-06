import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/' }),
    Outlet: () => <div data-testid="outlet" />,
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Header } from './header'

describe('Header', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('renders namespace selector', () => {
    render(<Header />)
    expect(screen.getByText('default')).toBeInTheDocument()
  })

  it('renders theme toggle button', () => {
    render(<Header />)
    // In light mode, Moon icon is shown; there are 2 buttons (theme toggle + logout)
    const buttons = screen.getAllByRole('button')
    expect(buttons.length).toBeGreaterThanOrEqual(2)
  })

  it('logout button calls clearToken on auth store', async () => {
    const user = userEvent.setup()
    render(<Header />)
    // Logout button is the last button
    const buttons = screen.getAllByRole('button')
    const logoutButton = buttons[buttons.length - 1]
    await user.click(logoutButton)
    expect(useAuthStore.getState().token).toBeNull()
  })
})
