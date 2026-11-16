import { describe, it, expect } from 'vitest'
import { render } from '@/test/test-utils'
import { OrcaMark } from './orca-mark'

describe('OrcaMark', () => {
  it('renders the full mark as a decorative SVG by default', () => {
    const { container } = render(<OrcaMark className="h-12 w-12" />)
    const svg = container.querySelector('svg')!
    expect(svg).toHaveAttribute('aria-hidden', 'true')
    expect(svg.getAttribute('class')).toContain('h-12')
    const ring = svg.querySelector(':scope > circle')!
    expect(ring).toHaveAttribute('r', '28.75')
    expect(ring).toHaveAttribute('stroke-width', '2.5')
  })

  it('renders the small variant with a heavier ring', () => {
    const { container } = render(<OrcaMark small />)
    const ring = container.querySelector('svg > circle')!
    expect(ring).toHaveAttribute('r', '28')
    expect(ring).toHaveAttribute('stroke-width', '4')
  })

  it('gives each instance its own clip and mask ids', () => {
    const { container } = render(
      <>
        <OrcaMark />
        <OrcaMark small />
      </>,
    )
    const marks = [...container.querySelectorAll('svg')]
    const ids = marks.map((svg) => {
      const clip = svg.querySelector('clipPath')!.id
      const mask = svg.querySelector('mask')!.id
      const group = svg.querySelector('g')!
      expect(group.getAttribute('clip-path')).toBe(`url(#${clip})`)
      expect(group.getAttribute('mask')).toBe(`url(#${mask})`)
      return [clip, mask]
    })
    expect(new Set(ids.flat()).size).toBe(4)
  })
})
