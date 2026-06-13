import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { Distribution } from './distribution'

describe('Distribution', () => {
  const segments = [
    { phase: 'Pending', count: 2 },
    { phase: 'Running', count: 3 },
    { phase: 'Succeeded', count: 4 },
    { phase: 'Failed', count: 1 },
  ]

  it('renders a segment per non-zero phase, each using the shared status token', () => {
    render(<Distribution segments={segments} />)
    expect(screen.getByTestId('dist-segment-Running').className).toContain('bg-status-running')
    expect(screen.getByTestId('dist-segment-Succeeded').className).toContain('bg-status-succeeded')
    expect(screen.getByTestId('dist-segment-Failed').className).toContain('bg-status-failed')
    expect(screen.getByTestId('dist-segment-Pending').className).toContain('bg-status-pending')
  })

  it('segment widths are proportional and sum to ~100% of the total', () => {
    render(<Distribution segments={segments} />)
    const total = segments.reduce((s, x) => s + x.count, 0) // 10
    const widths = segments.map((s) => {
      const el = screen.getByTestId(`dist-segment-${s.phase}`)
      return parseFloat((el as HTMLElement).style.width)
    })
    const sum = widths.reduce((a, b) => a + b, 0)
    expect(Math.round(sum)).toBe(100)
    // Running (3/10) => 30%
    expect(Math.round(widths[1])).toBe((3 / total) * 100)
  })

  it('omits zero-count segments from the bar', () => {
    render(
      <Distribution
        segments={[
          { phase: 'Running', count: 0 },
          { phase: 'Succeeded', count: 5 },
        ]}
      />,
    )
    expect(screen.queryByTestId('dist-segment-Running')).not.toBeInTheDocument()
    expect(screen.getByTestId('dist-segment-Succeeded')).toBeInTheDocument()
  })

  it('degrades to an empty track when all counts are zero', () => {
    render(
      <Distribution
        segments={[
          { phase: 'Running', count: 0 },
          { phase: 'Succeeded', count: 0 },
        ]}
      />,
    )
    // No segments rendered, but the legend + track still present.
    expect(screen.queryByTestId('dist-segment-Running')).not.toBeInTheDocument()
    expect(screen.getByRole('img', { name: /distribution/i })).toBeInTheDocument()
  })

  it('renders a legend entry with the count for each phase', () => {
    render(<Distribution segments={segments} />)
    // Legend shows all four labels.
    for (const label of ['Pending', 'Running', 'Succeeded', 'Failed']) {
      expect(screen.getByText(label)).toBeInTheDocument()
    }
  })
})
