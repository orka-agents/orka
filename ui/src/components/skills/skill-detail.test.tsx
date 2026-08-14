import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

const navigate = vi.fn()
vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => navigate,
    useLocation: () => ({ pathname: '/skills/code-review' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { SkillDetail } from './skill-detail'

describe('SkillDetail', () => {
  beforeEach(() => {
    navigate.mockReset()
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('renders skill metadata and served content', async () => {
    server.use(
      http.get('/api/v1/skills/:name', ({ params }) =>
        HttpResponse.json({
          metadata: { name: params.name, namespace: 'default' },
          spec: {
            displayName: 'Code Review',
            description: 'Structured review checklist',
            version: '1.2.0',
            tags: ['review'],
            content: { inline: '# Inline', files: { 'templates/checklist.md': '- [ ]' } },
          },
          status: { phase: 'Ready' },
        }),
      ),
      http.get('/api/v1/skills/:name/content', () =>
        new HttpResponse('# Served content', { headers: { 'Content-Type': 'text/markdown' } }),
      ),
    )
    render(<SkillDetail skillName="code-review" />)
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Code Review' })).toBeInTheDocument())
    expect(screen.getByText('v1.2.0')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByText('# Served content')).toBeInTheDocument())
    expect(screen.getByText('templates/checklist.md')).toBeInTheDocument()
  })

  it('deletes after confirm and navigates back', async () => {
    let deleted = false
    server.use(
      http.delete('/api/v1/skills/:name', () => {
        deleted = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<SkillDetail skillName="code-review" />)
    await waitFor(() => expect(screen.getByRole('heading', { name: /code-review/i })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /^delete$/i }))
    await user.click(screen.getByRole('button', { name: /confirm delete/i }))
    await waitFor(() => expect(deleted).toBe(true))
    expect(navigate).toHaveBeenCalledWith({ to: '/skills' })
  })
})
