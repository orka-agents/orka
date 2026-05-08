import { useState, useMemo } from 'react'
import { Link } from '@tanstack/react-router'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Badge } from '@/components/ui/badge'
import { ArrowUpDown, ArrowUp, ArrowDown } from 'lucide-react'
import type { SecurityFinding } from '@/schemas/security'

function severityVariant(severity?: string): 'destructive' | 'secondary' | 'outline' {
  if (severity === 'critical' || severity === 'high') return 'destructive'
  if (severity === 'medium') return 'secondary'
  return 'outline'
}

const SEVERITY_ORDER: Record<string, number> = { critical: 0, high: 1, medium: 2, low: 3 }

type SortKey = 'severity' | 'validationStatus' | 'state' | 'location' | 'title'
type SortDir = 'asc' | 'desc'

function compareFindingsByKey(a: SecurityFinding, b: SecurityFinding, key: SortKey): number {
  switch (key) {
    case 'severity':
      return (SEVERITY_ORDER[a.severity] ?? 99) - (SEVERITY_ORDER[b.severity] ?? 99)
    case 'validationStatus':
      return (a.validationStatus ?? '').localeCompare(b.validationStatus ?? '')
    case 'state':
      return (a.state ?? '').localeCompare(b.state ?? '')
    case 'location': {
      const locA = a.filePath ?? ''
      const locB = b.filePath ?? ''
      const fileCmp = locA.localeCompare(locB)
      if (fileCmp !== 0) return fileCmp
      return (a.line ?? 0) - (b.line ?? 0)
    }
    case 'title':
      return a.title.localeCompare(b.title)
    default:
      return 0
  }
}

function SortableHead({ label, sortKey, currentSort, currentDir, onSort }: {
  label: string
  sortKey: SortKey
  currentSort: SortKey | null
  currentDir: SortDir
  onSort: (key: SortKey) => void
}) {
  const Icon = currentSort === sortKey ? (currentDir === 'asc' ? ArrowUp : ArrowDown) : ArrowUpDown
  const ariaSort = currentSort === sortKey ? (currentDir === 'asc' ? 'ascending' as const : 'descending' as const) : undefined
  return (
    <TableHead aria-sort={ariaSort} className="whitespace-nowrap">
      <button
        type="button"
        className="inline-flex w-full items-center gap-1 cursor-pointer select-none"
        onClick={() => onSort(sortKey)}
      >
        {label}
        <Icon className="h-3 w-3 text-muted-foreground" />
      </button>
    </TableHead>
  )
}

export function FindingTable({ findings }: { findings: SecurityFinding[] }) {
  const [sortKey, setSortKey] = useState<SortKey | null>(null)
  const [sortDir, setSortDir] = useState<SortDir>('asc')

  const handleSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortDir((d) => (d === 'asc' ? 'desc' : 'asc'))
    } else {
      setSortKey(key)
      setSortDir('asc')
    }
  }

  const sorted = useMemo(() => {
    if (!sortKey) return findings
    const copy = [...findings]
    copy.sort((a, b) => {
      const cmp = compareFindingsByKey(a, b, sortKey)
      return sortDir === 'asc' ? cmp : -cmp
    })
    return copy
  }, [findings, sortKey, sortDir])

  if (findings.length === 0) {
    return <div className="py-8 text-sm text-muted-foreground">No findings to display.</div>
  }

  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <SortableHead label="Severity" sortKey="severity" currentSort={sortKey} currentDir={sortDir} onSort={handleSort} />
            <SortableHead label="Validation" sortKey="validationStatus" currentSort={sortKey} currentDir={sortDir} onSort={handleSort} />
            <SortableHead label="State" sortKey="state" currentSort={sortKey} currentDir={sortDir} onSort={handleSort} />
            <SortableHead label="Location" sortKey="location" currentSort={sortKey} currentDir={sortDir} onSort={handleSort} />
            <SortableHead label="Finding" sortKey="title" currentSort={sortKey} currentDir={sortDir} onSort={handleSort} />
          </TableRow>
        </TableHeader>
        <TableBody>
          {sorted.map((finding) => (
            <TableRow key={finding.id}>
              <TableCell><Badge variant={severityVariant(finding.severity)}>{finding.severity}</Badge></TableCell>
              <TableCell className="whitespace-nowrap">{finding.validationStatus}</TableCell>
              <TableCell className="whitespace-nowrap">{finding.state}</TableCell>
              <TableCell className="whitespace-nowrap text-xs font-mono">{finding.filePath ? `${finding.filePath}${finding.line ? `:${finding.line}` : ''}` : '-'}</TableCell>
              <TableCell className="max-w-md">
                <Link to="/security/findings/$findingId" params={{ findingId: finding.id }} className="font-medium text-primary hover:underline">
                  {finding.title}
                </Link>
                <div className="mt-1 text-xs text-muted-foreground truncate max-w-md">{finding.summary}</div>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
