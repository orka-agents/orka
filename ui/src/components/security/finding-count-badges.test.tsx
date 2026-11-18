import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { FindingCountBadges } from './finding-count-badges'

describe('FindingCountBadges', () => {
  it('renders zero counts for a repository without status', () => {
    render(<FindingCountBadges />)
    for (const label of ['0 critical', '0 high', '0 medium', '0 low']) {
      expect(screen.getByText(label)).not.toHaveAttribute('data-variant', 'destructive')
    }
  })

  it('marks critical and high red only when there are findings', () => {
    render(<FindingCountBadges counts={{ critical: 1, high: 0, medium: 3, low: 4 }} />)
    expect(screen.getByText('1 critical')).toHaveAttribute('data-variant', 'destructive')
    expect(screen.getByText('0 high')).toHaveAttribute('data-variant', 'outline')
    expect(screen.getByText('3 medium')).toHaveAttribute('data-variant', 'secondary')
    expect(screen.getByText('4 low')).toHaveAttribute('data-variant', 'outline')
  })
})
