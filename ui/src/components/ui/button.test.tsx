import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { Button } from './button'

describe('Button', () => {
  it('uses the theme destructive foreground for destructive buttons', () => {
    render(<Button variant="destructive">Delete</Button>)
    const button = screen.getByRole('button', { name: 'Delete' })
    expect(button).toHaveClass('bg-destructive', 'text-destructive-foreground')
    // The dark theme's destructive tokens already set contrast; no dark: override.
    expect(button.className).not.toMatch(/\btext-white\b|dark:bg-destructive/)
  })
})
