import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { act } from '@testing-library/react'

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
    expect(screen.getByText('default')).toBeInTheDocument()
    // Outlet is the main content area
    expect(screen.getByTestId('outlet')).toBeInTheDocument()
  })

  it('makes the page content inert while the mobile navigation drawer is open', () => {
    const original = window.matchMedia
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      matches: query === '(max-width: 767px)',
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia
    try {
      render(<RootLayout />)
      expect(screen.getByTestId('page-content')).not.toHaveAttribute('inert')
      act(() => useUIStore.setState({ mobileSidebarOpen: true }))
      expect(screen.getByTestId('page-content')).toHaveAttribute('inert')
      act(() => useUIStore.setState({ mobileSidebarOpen: false }))
      expect(screen.getByTestId('page-content')).not.toHaveAttribute('inert')
    } finally {
      window.matchMedia = original
    }
  })
})
