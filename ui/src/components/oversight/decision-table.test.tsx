import { describe, expect, it, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen } from '@/test/test-utils'
vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

import { useAuthStore } from '@/stores/auth'
import { DecisionTable } from './decision-table'
import { QuestionMatrix } from './question-matrix'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { charterConfig, charterDecision, makeDecision, riskyDecision, unavailableDecision } from '@/test/fixtures/oversight'

const rowOf = (signal: string) => document.querySelector<HTMLElement>(`tr[data-signal="${signal}"]`)!

describe('DecisionTable', () => {
  it('gives each row one badge, the most severe, in words', () => {
    const risky = makeDecision({ id: 'r@1', signal: 'risky', disposition: 'audit_flagged' })
    render(<DecisionTable decisions={[makeDecision(), riskyDecision, risky, charterDecision, unavailableDecision]} onOpen={() => {}} />)
    const badges = [...document.querySelectorAll('tbody tr')].map((row) => [...row.querySelectorAll('[data-badge]')].map((b) => b.textContent))
    expect(badges).toEqual([
      ['Consistent with the task'],
      ['Would pause (audit mode)'],
      ['Looks risky'],
      ['Against the charter'],
      ['Scorer unavailable → would hold'],
    ])
  })

  it('summarises each row in plain language from its fields', async () => {
    render(<DecisionTable decisions={[riskyDecision, charterDecision, unavailableDecision, makeDecision({ id: 's@1', actionNarrative: '[synthetic] Pushed an image.' })]} onOpen={() => {}} />)
    expect(screen.getByRole('button', { name: 'sent 2 KB to cdn.registry.example:443 — would pause' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Ran the test suite. — breaks charter rule B1' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Ran the test suite. — scorer unavailable, would hold' })).toBeInTheDocument()
    // The synthetic marker is stripped from rows.
    expect(screen.getByRole('button', { name: 'Pushed an image.' })).toBeInTheDocument()
    // Raw tool text and IDs are on hover, not in the row.
    const row = screen.getByRole('button', { name: /sent 2 KB/ })
    expect(row).toHaveAttribute('title', expect.stringContaining('request: fetch weights'))
    expect(row).toHaveAttribute('title', expect.stringContaining('task build-1'))
    expect(rowOf('risky')).not.toHaveTextContent('build-1')
    expect(rowOf('risky')).not.toHaveTextContent('code_exec')
  })

  it('names the declared rule number when the charter is declared', async () => {
    useAuthStore.setState({ token: 'test-token' })
    server.use(http.get('/api/v1/oversight/config', () => HttpResponse.json(charterConfig)))
    render(<DecisionTable decisions={[charterDecision]} onOpen={() => {}} />)
    expect(await screen.findByRole('button', { name: 'Ran the test suite. — breaks charter rule 2' })).toBeInTheDocument()
  })

  it('keeps grey rows quiet and opens a row on click', async () => {
    const onOpen = vi.fn()
    render(<DecisionTable decisions={[makeDecision(), riskyDecision]} onOpen={onOpen} />)
    expect(rowOf('consistent').className).toContain('text-muted-foreground')
    expect(rowOf('consistent').className).not.toContain('border-l-')
    await userEvent.click(screen.getByRole('button', { name: /sent 2 KB/ }))
    expect(onOpen).toHaveBeenCalledWith('build-1@2')
  })

  it('selects rows without opening them', async () => {
    const onOpen = vi.fn()
    const onToggle = vi.fn()
    render(<DecisionTable decisions={[riskyDecision]} onOpen={onOpen} selectedIds={new Set()} onToggleSelect={onToggle} />)
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select build-1@2' }))
    expect(onToggle).toHaveBeenCalledWith('build-1@2')
    expect(onOpen).not.toHaveBeenCalled()
  })
})

describe('QuestionMatrix', () => {
  it('puts questions in columns and highlights only triggered cells', async () => {
    const onOpen = vi.fn()
    render(<QuestionMatrix decisions={[makeDecision(), riskyDecision]} onOpen={onOpen} />)
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent)
    expect(headers).toEqual(['Action', 'Signal', 'K1', 'K2', 'K10'])
    const hot = document.querySelectorAll('td[data-triggered]')
    expect([...hot].map((cell) => cell.getAttribute('aria-label'))).toEqual([
      'K2: 0.91 against threshold 0.82, triggered',
      'K10: named, triggered',
    ])
    await userEvent.click(screen.getAllByRole('row')[2])
    expect(onOpen).toHaveBeenCalledWith('build-1@2')
  })
})
