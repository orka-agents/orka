import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return { ...actual, Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>, useNavigate: () => vi.fn(), useLocation: () => ({ pathname: '/tasks' }) }
})

import { TaskFilesChanged } from './task-files-changed'

describe('TaskFilesChanged', () => {
  it('renders a list of files', () => {
    render(<TaskFilesChanged files={['src/main.go', 'pkg/utils.go']} />)
    expect(screen.getByText('src/main.go')).toBeInTheDocument()
    expect(screen.getByText('pkg/utils.go')).toBeInTheDocument()
  })

  it('renders empty state', () => {
    render(<TaskFilesChanged files={[]} />)
    expect(screen.getByText('No files changed')).toBeInTheDocument()
  })

  it('renders file icons', () => {
    render(<TaskFilesChanged files={['file.ts']} />)
    const list = screen.getByTestId('files-changed')
    expect(list.querySelectorAll('svg').length).toBe(1)
  })
})
