import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Table, TableBody, TableCell, TableRow } from './table'

function renderTable(scrollRegionLabel?: string) {
  return render(
    <Table scrollRegionLabel={scrollRegionLabel}>
      <TableBody><TableRow><TableCell>cell</TableCell></TableRow></TableBody>
    </Table>,
  )
}

describe('Table', () => {
  it('adds no tab stop or region to its scroll container by default', () => {
    const { container } = renderTable()
    const scroller = container.querySelector('[data-slot="table-container"]')!
    expect(scroller).not.toHaveAttribute('tabindex')
    expect(scroller).not.toHaveAttribute('role')
    expect(screen.queryByRole('region')).not.toBeInTheDocument()
  })

  it('makes a labeled scroll container a focusable named region', () => {
    renderTable('Team summary')
    const region = screen.getByRole('region', { name: 'Team summary' })
    expect(region).toHaveAttribute('data-slot', 'table-container')
    expect(region).toHaveAttribute('tabindex', '0')
    expect(region).toContainElement(screen.getByRole('table'))
  })
})
