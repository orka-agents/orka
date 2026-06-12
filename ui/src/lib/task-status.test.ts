import { describe, it, expect } from 'vitest'
import { phaseStyle, typeStyle } from './task-status'

describe('phaseStyle', () => {
  it.each(['Pending', 'Running', 'Succeeded', 'Failed'] as const)(
    'returns token-based classes for %s (no pastel literals)',
    (phase) => {
      const s = phaseStyle(phase)
      expect(s.label).toBe(phase)
      expect(s.dotClass).toBe(`bg-status-${phase.toLowerCase()}`)
      expect(s.railClass).toBe(`border-status-${phase.toLowerCase()}`)
      expect(s.textClass).toBe(`text-status-${phase.toLowerCase()}`)
      expect(s.bgClass).toBe(`bg-status-${phase.toLowerCase()}-bg`)
      // Guard against any regression back to template pastels.
      expect(s.dotClass).not.toMatch(/-(100|200|800|900)\b/)
    },
  )

  it('marks only Running as live', () => {
    expect(phaseStyle('Running').live).toBe(true)
    for (const phase of ['Pending', 'Succeeded', 'Failed'] as const) {
      expect(phaseStyle(phase).live).toBe(false)
    }
  })

  it('falls back to Pending for unknown/undefined phases', () => {
    expect(phaseStyle(undefined).label).toBe('Pending')
    expect(phaseStyle('Nonsense').label).toBe('Pending')
  })
})

describe('typeStyle', () => {
  it.each(['container', 'ai', 'agent'] as const)(
    'returns an icon and token classes for %s',
    (type) => {
      const s = typeStyle(type)
      expect(s.label).toBe(type)
      expect(s.icon).toBeTruthy()
      expect(s.textClass).toBe(`text-type-${type}`)
      expect(s.tintClass).toContain(`bg-type-${type}`)
    },
  )

  it('falls back to container for unknown/undefined types', () => {
    expect(typeStyle(undefined).label).toBe('container')
    expect(typeStyle('weird').label).toBe('container')
  })
})
