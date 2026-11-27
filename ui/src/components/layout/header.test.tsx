import { describe, it, expect, beforeAll, beforeEach, vi } from 'vitest'
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
  beforeAll(() => {
    if (!HTMLElement.prototype.hasPointerCapture) HTMLElement.prototype.hasPointerCapture = () => false
    if (!HTMLElement.prototype.setPointerCapture) HTMLElement.prototype.setPointerCapture = () => {}
    if (!HTMLElement.prototype.releasePointerCapture) HTMLElement.prototype.releasePointerCapture = () => {}
    if (!HTMLElement.prototype.scrollIntoView) HTMLElement.prototype.scrollIntoView = () => {}
  })

  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default', recentNamespaces: ['default'], callerNamespace: null })
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

  it("offers the caller's and recent namespaces instead of two fixed entries", async () => {
    const user = userEvent.setup()
    useUIStore.setState({ namespace: 'team-a', recentNamespaces: ['team-a', 'team-b'], callerNamespace: 'team-a' })
    render(<Header />)
    await user.click(screen.getByRole('combobox', { name: 'Namespace' }))
    const options = await screen.findAllByRole('option')
    expect(options.map((option) => option.textContent)).toEqual(['team-a', 'team-b', 'Other namespace…'])

    await user.click(screen.getByRole('option', { name: 'team-b' }))
    expect(useUIStore.getState().namespace).toBe('team-b')
  })

  it('switches to a typed namespace and remembers it', async () => {
    const user = userEvent.setup()
    render(<Header />)
    await user.click(screen.getByRole('combobox', { name: 'Namespace' }))
    await user.click(await screen.findByRole('option', { name: 'Other namespace…' }))
    await user.type(screen.getByRole('textbox', { name: 'Namespace name' }), 'team-c{Enter}')

    expect(useUIStore.getState().namespace).toBe('team-c')
    expect(useUIStore.getState().recentNamespaces).toEqual(['team-c', 'default'])
    expect(screen.getByRole('combobox', { name: 'Namespace' })).toHaveTextContent('team-c')
  })

  it('rejects a typed namespace that is not a DNS-1123 label', async () => {
    const user = userEvent.setup()
    render(<Header />)
    await user.click(screen.getByRole('combobox', { name: 'Namespace' }))
    await user.click(await screen.findByRole('option', { name: 'Other namespace…' }))
    const input = screen.getByRole('textbox', { name: 'Namespace name' })
    await user.type(input, 'Team_A{Enter}')

    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByRole('alert')).toHaveTextContent('DNS-1123')
    expect(useUIStore.getState().namespace).toBe('default')

    await user.keyboard('{Escape}')
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
