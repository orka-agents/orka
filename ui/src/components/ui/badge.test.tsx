import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { Badge } from './badge'

describe('Badge', () => {
  it('uses the theme destructive foreground for destructive badges', () => {
    render(<Badge variant="destructive">3 critical</Badge>)
    const badge = screen.getByText('3 critical')
    expect(badge).toHaveClass('bg-destructive', 'text-destructive-foreground')
    expect(badge.className).not.toMatch(/\btext-white\b|dark:bg-destructive/)
  })
})
