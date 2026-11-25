import { describe, expect, it, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { render, screen, within } from '@/test/test-utils'
import { DecisionTable } from './decision-table'
import { QuestionMatrix } from './question-matrix'
import { charterDecision, makeDecision, riskyDecision, unavailableDecision } from '@/test/fixtures/oversight'

const rowOf = (signal: string) => document.querySelector<HTMLElement>(`tr[data-signal="${signal}"]`)!

describe('DecisionTable', () => {
  it('labels every signal in words, not colour alone', () => {
    render(<DecisionTable decisions={[makeDecision(), riskyDecision, charterDecision, unavailableDecision]} onOpen={() => {}} />)
    expect(within(rowOf('consistent')).getAllByText('Consistent with the task').length).toBeGreaterThan(0)
    expect(within(rowOf('risky')).getByText('Looks risky')).toBeInTheDocument()
    expect(within(rowOf('risky')).getByText('K2 0.91 ≥ 0.82')).toBeInTheDocument()
    expect(within(rowOf('against_charter')).getByText('Against the charter')).toBeInTheDocument()
    expect(within(rowOf('against_charter')).getByText('Never write outside the workspace.')).toBeInTheDocument()
    expect(within(rowOf('monitor_unavailable')).getByText('Scorer unavailable → would hold')).toBeInTheDocument()
    expect(within(rowOf('monitor_unavailable')).getByText(/never a silent pass/)).toBeInTheDocument()
  })

  it('shows the would-pause chip only when gate mode would have paused', () => {
    render(<DecisionTable decisions={[makeDecision(), riskyDecision]} onOpen={() => {}} />)
    expect(within(rowOf('risky')).getByText('Would pause (audit mode)')).toBeInTheDocument()
    expect(within(rowOf('consistent')).queryByText('Would pause (audit mode)')).not.toBeInTheDocument()
  })

  it('keeps grey rows quiet and opens a row on click', async () => {
    const onOpen = vi.fn()
    render(<DecisionTable decisions={[makeDecision(), riskyDecision]} onOpen={onOpen} />)
    expect(rowOf('consistent').className).toContain('text-muted-foreground')
    expect(rowOf('consistent').className).not.toContain('border-l-')
    await userEvent.click(screen.getByRole('button', { name: /Inspect code_exec action build-1@2/ }))
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
