import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import path from 'node:path'

const html = readFileSync(path.resolve(__dirname, '../index.html'), 'utf8')
const prePaint = new DOMParser().parseFromString(html, 'text/html').querySelector('script:not([src])')!.textContent!

function paintsDark(stored: string | null, systemDark: boolean): boolean {
  document.documentElement.classList.remove('dark')
  const localStorage = { getItem: () => stored }
  const window = { matchMedia: () => ({ matches: systemDark }) }
  new Function('localStorage', 'window', 'document', prePaint)(localStorage, window, document)
  return document.documentElement.classList.contains('dark')
}

const saved = (theme: string, version?: number) =>
  JSON.stringify({ state: { theme }, ...(version !== undefined && { version }) })

describe('index.html pre-paint theme', () => {
  it('follows the system for a first visit, a System pick, or unreadable storage', () => {
    expect(paintsDark(null, true)).toBe(true)
    expect(paintsDark(null, false)).toBe(false)
    expect(paintsDark(saved('system', 1), true)).toBe(true)
    expect(paintsDark('not json', true)).toBe(true)
  })

  it('treats a stored light from before version 1 as the old default, as the store does', () => {
    expect(paintsDark(saved('light', 0), true)).toBe(true)
    expect(paintsDark(saved('light'), true)).toBe(false)
  })

  it('keeps an explicit pick', () => {
    expect(paintsDark(saved('light', 1), true)).toBe(false)
    expect(paintsDark(saved('dark', 1), false)).toBe(true)
    expect(paintsDark(saved('dark'), false)).toBe(true)
  })
})
