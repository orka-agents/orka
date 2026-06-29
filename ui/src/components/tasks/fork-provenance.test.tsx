import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { ForkProvenance } from './fork-provenance'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, params, ...props }: any) => {
      let href = to
      if (typeof to === 'string' && params) {
        for (const [k, v] of Object.entries(params)) href = href.replace(`$${k}`, v as string)
      }
      return <a href={href} {...props}>{children}</a>
    },
  }
})

describe('ForkProvenance', () => {
  it('renders nothing for a non-forked task', () => {
    const { container } = render(<ForkProvenance annotations={{}} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when annotations are absent', () => {
    const { container } = render(<ForkProvenance />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows the source task as a link and the source seq', () => {
    render(
      <ForkProvenance
        annotations={{
          'orka.ai/fork-source-task': 'parent-task',
          'orka.ai/fork-source-seq': '7',
        }}
      />,
    )
    expect(screen.getByTestId('fork-provenance')).toBeInTheDocument()
    const link = screen.getByRole('link', { name: /parent-task/ })
    expect(link).toHaveAttribute('href', '/tasks/parent-task')
    expect(screen.getByText('after #7')).toBeInTheDocument()
  })

  it('notes when the forked context was truncated', () => {
    render(
      <ForkProvenance
        annotations={{
          'orka.ai/fork-source-task': 'parent-task',
          'orka.ai/fork-source-seq': '7',
          'orka.ai/fork-context-truncated': 'true',
        }}
      />,
    )
    expect(screen.getByText(/context was truncated/i)).toBeInTheDocument()
  })
})
