import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/skills' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { SkillList } from './skill-list'

describe('SkillList', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('shows an empty state', async () => {
    render(<SkillList />)
    await waitFor(() => expect(screen.getByText(/no skills yet/i)).toBeInTheDocument())
  })

  it('lists skills with tags and phase', async () => {
    server.use(
      http.get('/api/v1/skills', () =>
        HttpResponse.json({
          items: [
            {
              name: 'code-review',
              namespace: 'default',
              displayName: 'Code Review',
              description: 'Structured review checklist',
              version: '1.2.0',
              tags: ['review', 'golang'],
              phase: 'Ready',
            },
          ],
          metadata: {},
        }),
      ),
    )
    render(<SkillList />)
    await waitFor(() => expect(screen.getByText('Code Review')).toBeInTheDocument())
    expect(screen.getByText('review')).toBeInTheDocument()
    expect(screen.getByText('1.2.0')).toBeInTheDocument()
    expect(screen.getByText('Ready')).toBeInTheDocument()
  })

  it('offers a create action', () => {
    render(<SkillList />)
    expect(screen.getByRole('link', { name: /new skill/i })).toHaveAttribute('href', '/skills/new')
  })
})
