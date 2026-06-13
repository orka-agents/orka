import { describe, it, expect } from 'vitest'
import { render } from '@/test/test-utils'
import { SonarPing } from './sonar-ping'

describe('SonarPing', () => {
  it('renders a decorative, aria-hidden sonar illustration', () => {
    const { container } = render(<SonarPing />)
    const root = container.firstElementChild as HTMLElement
    expect(root).toBeInTheDocument()
    expect(root).toHaveAttribute('aria-hidden', 'true')
    // Concentric rings + center dot => multiple spans.
    expect(root.querySelectorAll('span').length).toBeGreaterThanOrEqual(3)
  })

  it('applies an extra className when provided', () => {
    const { container } = render(<SonarPing className="my-4" />)
    expect((container.firstElementChild as HTMLElement).className).toContain('my-4')
  })
})
