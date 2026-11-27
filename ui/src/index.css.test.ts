import { describe, it, expect } from 'vitest'
import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'

const css = readFileSync(
  path.resolve(__dirname, './index.css'),
  'utf8',
)

function block(selector: string): string {
  const start = css.indexOf(selector)
  expect(start, `selector ${selector} present`).toBeGreaterThan(-1)
  const open = css.indexOf('{', start)
  let depth = 0
  for (let i = open; i < css.length; i++) {
    if (css[i] === '{') depth++
    else if (css[i] === '}') {
      depth--
      if (depth === 0) return css.slice(open + 1, i)
    }
  }
  throw new Error(`unbalanced block for ${selector}`)
}

function tokenValue(body: string, name: string): string | undefined {
  const m = body.match(new RegExp(`${name.replace(/[-]/g, '\\-')}\\s*:\\s*([^;]+);`))
  return m?.[1].trim()
}

function contrast(a: string, b: string): number {
  const luminance = (hex: string) => {
    const [r, g, bl] = [1, 3, 5].map((i) => {
      const c = parseInt(hex.slice(i, i + 2), 16) / 255
      return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
    })
    return 0.2126 * r + 0.7152 * g + 0.0722 * bl
  }
  const [high, low] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (high + 0.05) / (low + 0.05)
}

function over(color: string, base: string, alpha: number): string {
  const channel = (hex: string, i: number) => parseInt(hex.slice(i, i + 2), 16)
  return `#${[1, 3, 5]
    .map((i) => Math.round(channel(color, i) * alpha + channel(base, i) * (1 - alpha)))
    .map((c) => c.toString(16).padStart(2, '0'))
    .join('')}`
}

describe('index.css design tokens', () => {
  const themeBody = block('@theme')
  const darkBody = block('\n.dark {')

  it('defines every CSS variable the components read', () => {
    const defined = new Set([...css.matchAll(/(--[\w-]+)\s*:/g)].map((m) => m[1]))
    const sources = readdirSync(__dirname, { recursive: true, encoding: 'utf8' })
      .filter((file) => /\.(tsx?|css)$/.test(file) && !/\.test\.tsx?$/.test(file))
    for (const file of sources) {
      const text = readFileSync(path.resolve(__dirname, file), 'utf8')
      for (const [, name] of text.matchAll(/var\((--[\w-]+)/g)) {
        if (name.startsWith('--radix-')) continue
        expect(defined.has(name), `${file} reads ${name}`).toBe(true)
      }
    }
  })

  it('card surface is distinct from the page background in light theme', () => {
    const card = tokenValue(themeBody, '--color-card')
    const bg = tokenValue(themeBody, '--color-background')
    expect(card).toBeTruthy()
    expect(bg).toBeTruthy()
    expect(card).not.toBe(bg)
  })

  it('card surface is distinct from the page background in dark theme', () => {
    const card = tokenValue(darkBody, '--color-card')
    const bg = tokenValue(darkBody, '--color-background')
    expect(card).toBeTruthy()
    expect(bg).toBeTruthy()
    expect(card).not.toBe(bg)
  })

  it('declares Geist as the sans font', () => {
    expect(themeBody).toMatch(/--font-sans:\s*"Geist Variable"/)
  })

  it('defines a monospace font token for machine data', () => {
    expect(tokenValue(themeBody, '--font-mono')).toMatch(/Geist Mono Variable/)
  })

  it('defines the four phase status tokens in both themes', () => {
    for (const body of [themeBody, darkBody]) {
      for (const phase of ['pending', 'running', 'succeeded', 'failed']) {
        expect(
          tokenValue(body, `--color-status-${phase}`),
          `--color-status-${phase}`,
        ).toBeTruthy()
      }
    }
  })

  it('reserves a liveness token in both themes', () => {
    expect(tokenValue(themeBody, '--color-live')).toBeTruthy()
    expect(tokenValue(darkBody, '--color-live')).toBeTruthy()
  })

  it('defines task-type tokens in both themes', () => {
    for (const body of [themeBody, darkBody]) {
      for (const type of ['container', 'ai', 'agent']) {
        expect(
          tokenValue(body, `--color-type-${type}`),
          `--color-type-${type}`,
        ).toBeTruthy()
      }
    }
  })

  it('gives native controls the color scheme of the app theme', () => {
    expect(tokenValue(block('\n:root {'), 'color-scheme')).toBe('light')
    expect(tokenValue(darkBody, 'color-scheme')).toBe('dark')
    expect(css.indexOf('\n:root {')).toBeLessThan(css.indexOf('\n.dark {'))
  })

  it.each(['status-pending', 'status-running', 'status-finalizing', 'status-succeeded', 'status-failed', 'live'])(
    'keeps light-theme --color-%s text at WCAG AA contrast on cards and the page',
    (name) => {
      const color = tokenValue(themeBody, `--color-${name}`)!
      expect(contrast(color, tokenValue(themeBody, '--color-card')!)).toBeGreaterThanOrEqual(4.5)
      expect(contrast(color, tokenValue(themeBody, '--color-background')!)).toBeGreaterThanOrEqual(4.5)
    },
  )

  it.each(['container', 'ai', 'agent'])(
    'keeps --color-type-%s badge text at WCAG AA contrast on cards and its own tint, in both themes',
    (type) => {
      for (const body of [themeBody, darkBody]) {
        const color = tokenValue(body, `--color-type-${type}`)!
        const card = tokenValue(body, '--color-card')!
        expect(contrast(color, card)).toBeGreaterThanOrEqual(4.5)
        expect(contrast(color, over(color, card, 0.1))).toBeGreaterThanOrEqual(4.5)
      }
    },
  )

  it('honors reduced motion globally', () => {
    expect(css).toMatch(/prefers-reduced-motion:\s*reduce/)
  })
})
