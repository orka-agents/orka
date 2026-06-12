import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { Sparkline } from './sparkline'

describe('Sparkline', () => {
  it('renders a polyline from a numeric series', () => {
    const { container } = render(<Sparkline data={[1, 5, 2, 8, 3]} />)
    const poly = container.querySelector('polyline')
    expect(poly).toBeInTheDocument()
    // 5 points => 5 coordinate pairs
    expect(poly!.getAttribute('points')!.trim().split(' ')).toHaveLength(5)
  })

  it('degrades gracefully on an empty series (no polyline, valid svg)', () => {
    const { container } = render(<Sparkline data={[]} />)
    expect(container.querySelector('svg')).toBeInTheDocument()
    expect(container.querySelector('polyline')).not.toBeInTheDocument()
  })

  it('renders a flat line for a single point', () => {
    const { container } = render(<Sparkline data={[7]} />)
    const poly = container.querySelector('polyline')
    expect(poly).toBeInTheDocument()
    expect(poly!.getAttribute('points')!.trim().split(' ')).toHaveLength(2)
  })

  it('exposes an accessible label', () => {
    render(<Sparkline data={[1, 2, 3]} aria-label="Throughput" />)
    expect(screen.getByRole('img', { name: 'Throughput' })).toBeInTheDocument()
  })
})
