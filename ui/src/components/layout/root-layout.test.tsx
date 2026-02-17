import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

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

vi.mock('next-themes', () => ({
  useTheme: () => ({ theme: 'light' }),
}))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { RootLayout } from './root-layout'

describe('RootLayout', () => {
  beforeEach(() => {
    useUIStore.setState({ sidebarCollapsed: false, theme: 'light', namespace: 'default' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('renders without crashing', () => {
    render(<RootLayout />)
    expect(screen.getByTestId('outlet')).toBeInTheDocument()
  })

  it('contains sidebar, header, and main content area', () => {
    render(<RootLayout />)
    // Sidebar renders nav items
    expect(screen.getByText('Dashboard')).toBeInTheDocument()
    // Header renders namespace selector
    expect(screen.getByLabelText('Namespace')).toHaveValue('default')
    // Outlet is the main content area
    expect(screen.getByTestId('outlet')).toBeInTheDocument()
  })
})
