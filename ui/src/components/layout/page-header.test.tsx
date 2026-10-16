import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { PageHeader } from './page-header'

describe('PageHeader', () => {
  it('renders the title as a level-1 heading', () => {
    render(<PageHeader title="Tasks" />)
    expect(screen.getByRole('heading', { level: 1, name: 'Tasks' })).toBeInTheDocument()
  })

  it('wraps a long unbroken title instead of widening the page', () => {
    const title = 'z'.repeat(72)
    render(<PageHeader title={title} />)
    const heading = screen.getByRole('heading', { level: 1, name: title })
    expect(heading).toHaveClass('break-words')
    expect(heading.parentElement).toHaveClass('min-w-0')
    expect(heading.parentElement?.parentElement).toHaveClass('min-w-0')
  })

  it('names the page in the browser tab until it unmounts', () => {
    const { unmount } = render(<PageHeader title="Tasks" />)
    expect(document.title).toBe('Tasks · Orka')
    unmount()
    expect(document.title).toBe('Orka')
  })

  it('renders the description when provided', () => {
    render(<PageHeader title="Tasks" description="Manage your task execution" />)
    expect(screen.getByText('Manage your task execution')).toBeInTheDocument()
  })

  it('omits the description when not provided', () => {
    const { container } = render(<PageHeader title="Tasks" />)
    expect(container.querySelectorAll('p').length).toBe(0)
  })

  it('renders the eyebrow when provided', () => {
    render(<PageHeader title="Detail" eyebrow="Task" />)
    expect(screen.getByText('Task')).toBeInTheDocument()
  })

  it('renders an action slot when provided and wires its handler', async () => {
    const onClick = vi.fn()
    render(
      <PageHeader
        title="Tasks"
        action={<button onClick={onClick}>New Task</button>}
      />,
    )
    const btn = screen.getByRole('button', { name: 'New Task' })
    expect(btn).toBeInTheDocument()
    await userEvent.click(btn)
    expect(onClick).toHaveBeenCalledOnce()
  })
})
