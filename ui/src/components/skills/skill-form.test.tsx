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
    useLocation: () => ({ pathname: '/skills/new' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { SkillForm } from './skill-form'
import type { Skill } from '@/schemas/skill'

describe('SkillForm', () => {
  beforeEach(() => {
    navigate.mockReset()
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('creates a skill with parsed tags', async () => {
    let posted: any
    server.use(
      http.post('/api/v1/skills', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: posted.name, namespace: 'default' }, spec: posted.spec }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<SkillForm />)
    await user.type(screen.getByLabelText(/^name$/i), 'code-review')
    await user.type(screen.getByLabelText(/^description$/i), 'Structured review')
    await user.type(screen.getByLabelText(/tags/i), 'review, golang ,')
    await user.type(screen.getByLabelText(/skill\.md content/i), '# Review checklist')
    await user.click(screen.getByRole('button', { name: /create skill/i }))
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: '/skills' }))
    expect(posted.spec.tags).toEqual(['review', 'golang'])
    expect(posted.spec.content.inline).toBe('# Review checklist')
  })

  it('requires description and content', async () => {
    const user = userEvent.setup()
    render(<SkillForm />)
    await user.type(screen.getByLabelText(/^name$/i), 'x')
    await user.click(screen.getByRole('button', { name: /create skill/i }))
    expect(navigate).not.toHaveBeenCalled()
  })

  it('edits an existing skill preserving source metadata', async () => {
    let putBody: any
    server.use(
      http.put('/api/v1/skills/:name', async ({ request, params }) => {
        putBody = await request.json()
        return HttpResponse.json({ metadata: { name: params.name, namespace: 'default' }, spec: putBody.spec })
      }),
    )
    const initial: Skill = {
      metadata: { name: 'code-review', namespace: 'default' },
      spec: {
        description: 'old',
        content: { inline: '# Old' },
        source: { github: '/anthropics/skills', skillName: 'code-review' },
      },
    }
    const user = userEvent.setup()
    render(<SkillForm initial={initial} />)
    const description = screen.getByLabelText(/^description$/i)
    await user.clear(description)
    await user.type(description, 'newer description')
    await user.click(screen.getByRole('button', { name: /save changes/i }))
    await waitFor(() =>
      expect(navigate).toHaveBeenCalledWith({ to: '/skills/$skillName', params: { skillName: 'code-review' } }),
    )
    expect(putBody.spec.description).toBe('newer description')
    expect(putBody.spec.source.github).toBe('/anthropics/skills')
  })
})
