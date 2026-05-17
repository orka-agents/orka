import { describe, expect, it, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import type { ReactNode } from 'react'
import { render, screen, within } from '@/test/test-utils'
import type { SecurityFinding } from '@/schemas/security'
import { FindingTable } from './finding-table'

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to }: { children: ReactNode; to: string }) => <a href={to}>{children}</a>,
  }
})

function makeFinding(overrides: Partial<SecurityFinding>): SecurityFinding {
  const id = overrides.id ?? 'finding'
  return {
    id,
    namespace: 'default',
    repositoryScan: 'repo',
    fingerprint: `fingerprint-${id}`,
    title: `Finding ${id}`,
    summary: `Summary ${id}`,
    severity: 'low',
    confidence: 'medium',
    validationStatus: 'unknown',
    state: 'open',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

describe('FindingTable', () => {
  it('sorts locations by file path and line number', async () => {
    const user = userEvent.setup()
    render(
      <FindingTable
        findings={[
          makeFinding({ id: 'line-90', filePath: 'foo.go', line: 90 }),
          makeFinding({ id: 'line-12', filePath: 'foo.go', line: 12 }),
        ]}
      />,
    )

    await user.click(screen.getByRole('button', { name: /location/i }))

    const rows = screen.getAllByRole('row').slice(1)
    expect(within(rows[0]).getByText('foo.go:12')).toBeInTheDocument()
    expect(within(rows[1]).getByText('foo.go:90')).toBeInTheDocument()
  })
})
