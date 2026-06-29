import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', async () => {
  const actual = await vi.importActual('zustand/middleware')
  return { ...actual, persist: (fn: any) => fn }
})

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    createFileRoute: (_path: string) => (opts: any) => ({
      ...opts,
      path: _path,
      useParams: () => ({ taskId: 'test-task-123' }),
    }),
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/tasks' }),
    useSearch: () => ({}),
  }
})

vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { Route as TaskIndexRoute } from './index'
import { Route as TaskNewRoute } from './new'
import { Route as TaskDetailRoute } from './$taskId'

describe('tasks routes', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  describe('tasks/index', () => {
    it('exports Route with component', () => {
      expect(TaskIndexRoute).toBeDefined()
      expect(TaskIndexRoute.component).toBeDefined()
      expect(TaskIndexRoute.path).toBe('/tasks/')
    })

    it('renders TaskList component', () => {
      const Component = TaskIndexRoute.component!
      render(<Component />)
      expect(screen.getByText('Tasks')).toBeInTheDocument()
      expect(screen.getByText('Manage your task execution')).toBeInTheDocument()
    })
  })

  describe('tasks/new', () => {
    it('exports Route with component', () => {
      expect(TaskNewRoute).toBeDefined()
      expect(TaskNewRoute.component).toBeDefined()
      expect(TaskNewRoute.path).toBe('/tasks/new')
    })

    it('renders TaskCreateForm component', () => {
      const Component = TaskNewRoute.component!
      render(<Component />)
      expect(screen.getByRole('heading', { name: 'Create Task' })).toBeInTheDocument()
    })
  })

  describe('tasks/$taskId', () => {
    it('exports Route with component', () => {
      expect(TaskDetailRoute).toBeDefined()
      expect(TaskDetailRoute.component).toBeDefined()
      expect(TaskDetailRoute.path).toBe('/tasks/$taskId')
    })

    it('renders TaskDetail with taskId param', () => {
      const Component = TaskDetailRoute.component!
      render(<Component />)
      // Task detail fetches data; in loading state it shows skeletons
      // The component is rendered without errors
      expect(document.querySelector('.animate-pulse')).toBeInTheDocument()
    })
  })
})
