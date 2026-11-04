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

  it('names the namespace selector', () => {
    render(<Header />)
    expect(screen.getByRole('combobox', { name: 'Namespace' })).toHaveTextContent('default')
  })

  it('exposes accessible names on the icon-only theme + logout buttons', () => {
    render(<Header />)
    expect(screen.getByRole('button', { name: 'Theme: Light' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /log out/i })).toBeInTheDocument()
  })

  it('offers System, Light and Dark, and records the pick', async () => {
    const user = userEvent.setup()
    useUIStore.setState({ theme: 'system' })
    render(<Header />)
    await user.click(screen.getByRole('button', { name: 'Theme: System' }))
    const options = await screen.findAllByRole('menuitemradio')
    expect(options.map((option) => option.textContent)).toEqual(['System', 'Light', 'Dark'])
    expect(screen.getByRole('menuitemradio', { name: 'System' })).toHaveAttribute('aria-checked', 'true')

    await user.click(screen.getByRole('menuitemradio', { name: 'Dark' }))
    expect(useUIStore.getState().theme).toBe('dark')
    expect(screen.getByRole('button', { name: 'Theme: Dark' })).toBeInTheDocument()
  })

  it('logout button calls clearToken on auth store', async () => {
    const user = userEvent.setup()
    render(<Header />)
    await user.click(screen.getByRole('button', { name: /log out/i }))
    expect(useAuthStore.getState().token).toBeNull()
  })
})
