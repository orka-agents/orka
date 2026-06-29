import { describe, it, expect } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { SeverityIcon } from './severity-icon'

describe('SeverityIcon', () => {
  it('emits an accessible label so severity is not conveyed by color alone', () => {
    render(<SeverityIcon severity="error" />)
    // The visible glyph is aria-hidden; an sr-only text label carries the meaning.
    expect(screen.getByText('Error')).toBeInTheDocument()
  })

  it('maps each known severity to a distinct label', () => {
    const { rerender } = render(<SeverityIcon severity="info" />)
    expect(screen.getByText('Info')).toBeInTheDocument()
    rerender(<SeverityIcon severity="warning" />)
    expect(screen.getByText('Warning')).toBeInTheDocument()
    rerender(<SeverityIcon severity="debug" />)
    expect(screen.getByText('Debug')).toBeInTheDocument()
  })

  it('defaults unknown severities to Info', () => {
    render(<SeverityIcon severity="totally-unknown" />)
    expect(screen.getByText('Info')).toBeInTheDocument()
  })
})
