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
import { Sidebar } from './sidebar'

describe('Sidebar', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('renders all 6 nav items', () => {
    render(<Sidebar />)
    expect(screen.getByText('Dashboard')).toBeInTheDocument()
    expect(screen.getByText('Chat')).toBeInTheDocument()
    expect(screen.getByText('Tasks')).toBeInTheDocument()
    expect(screen.getByText('Sessions')).toBeInTheDocument()
    expect(screen.getByText('Runtimes')).toBeInTheDocument()
    expect(screen.getByText('Agents')).toBeInTheDocument()
    expect(screen.getByText('Tools')).toBeInTheDocument()
  })

  it('active nav item has correct styling', () => {
    render(<Sidebar />)
    const dashboardLink = screen.getByText('Dashboard').closest('a')
    expect(dashboardLink?.className).toContain('bg-primary')
    const tasksLink = screen.getByText('Tasks').closest('a')
    expect(tasksLink?.className).not.toContain('bg-primary')
  })

  it('collapsed sidebar hides labels', () => {
    useUIStore.setState({ sidebarCollapsed: true })
    render(<Sidebar />)
    expect(screen.queryByText('Dashboard')).not.toBeInTheDocument()
    expect(screen.queryByText('Chat')).not.toBeInTheDocument()
    expect(screen.queryByText('Tasks')).not.toBeInTheDocument()
  })

  it('collapsed navigation links keep an accessible name', () => {
    useUIStore.setState({ sidebarCollapsed: true })
    render(<Sidebar />)
    expect(screen.getByRole('link', { name: 'Tasks' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Dashboard' })).toHaveAttribute('title', 'Dashboard')
  })

  it('toggle button collapses sidebar', async () => {
    const user = userEvent.setup()
    render(<Sidebar />)
    expect(screen.getByText('Dashboard')).toBeInTheDocument()
    const toggleButton = screen.getByRole('button', { name: /collapse sidebar/i })
    await user.click(toggleButton)
    expect(useUIStore.getState().sidebarCollapsed).toBe(true)
  })

  it('auto-collapses below the md breakpoint and overlays when expanded', async () => {
    const original = window.matchMedia
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      matches: query === '(max-width: 767px)',
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia
    try {
      const user = userEvent.setup()
      render(<Sidebar />)
      // The narrow viewport renders the icon rail without touching the
      // persisted desktop preference ("expanded").
      expect(screen.getByRole('complementary').className).toContain('w-16')
      expect(useUIStore.getState().sidebarCollapsed).toBe(false)
      expect(useUIStore.getState().mobileSidebarOpen).toBe(false)
      expect(screen.queryByTestId('sidebar-backdrop')).not.toBeInTheDocument()

      await user.click(screen.getByRole('button', { name: /expand sidebar/i }))
      const drawer = screen.getByRole('dialog', { name: 'Navigation' })
      expect(drawer.className).toContain('max-md:fixed')
      expect(drawer).toHaveAttribute('aria-modal', 'true')
      expect(drawer.contains(document.activeElement)).toBe(true)
      expect(screen.getByTestId('sidebar-backdrop')).toBeInTheDocument()
      expect(useUIStore.getState().sidebarCollapsed).toBe(false)

      // Tab wraps inside the drawer instead of reaching the covered page: from
      // the last nav link forward to the first focusable (the logo link), and
      // back again with Shift+Tab.
      const links = screen.getAllByRole('link')
      links[links.length - 1].focus()
      await user.tab()
      expect(drawer.contains(document.activeElement)).toBe(true)
      expect(document.activeElement).toBe(links[0])
      await user.tab({ shift: true })
      expect(document.activeElement).toBe(links[links.length - 1])

      // Escape closes the drawer and focus returns to the toggle that opened it.
      await user.keyboard('{Escape}')
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
      expect(document.activeElement).toBe(screen.getByRole('button', { name: /expand sidebar/i }))

      await user.click(screen.getByRole('button', { name: /expand sidebar/i }))

      // Navigating from the overlay closes it.
      await user.click(screen.getByText('Tasks'))
      expect(useUIStore.getState().mobileSidebarOpen).toBe(false)
      expect(screen.queryByTestId('sidebar-backdrop')).not.toBeInTheDocument()

      await user.click(screen.getByRole('button', { name: /expand sidebar/i }))
      await user.click(screen.getByTestId('sidebar-backdrop'))
      expect(useUIStore.getState().mobileSidebarOpen).toBe(false)
      // The desktop preference survived the whole mobile session.
      expect(useUIStore.getState().sidebarCollapsed).toBe(false)
    } finally {
      window.matchMedia = original
    }
  })

  it('collapse toggle exposes an accessible name reflecting its state', () => {
    useUIStore.setState({ sidebarCollapsed: true })
    render(<Sidebar />)
    expect(
      screen.getByRole('button', { name: /expand sidebar/i }),
    ).toBeInTheDocument()
  })
})
