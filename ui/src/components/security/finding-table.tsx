import { Link } from '@tanstack/react-router'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Badge } from '@/components/ui/badge'
import type { SecurityFinding } from '@/schemas/security'

function severityVariant(severity?: string): 'destructive' | 'secondary' | 'outline' {
  if (severity === 'critical' || severity === 'high') return 'destructive'
  if (severity === 'medium') return 'secondary'
  return 'outline'
}

export function FindingTable({ findings }: { findings: SecurityFinding[] }) {
  if (findings.length === 0) {
    return <div className="py-8 text-sm text-muted-foreground">No findings to display.</div>
  }

  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Finding</TableHead>
            <TableHead>Severity</TableHead>
            <TableHead>Validation</TableHead>
            <TableHead>State</TableHead>
            <TableHead>Location</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {findings.map((finding) => (
            <TableRow key={finding.id}>
              <TableCell className="min-w-80">
                <Link to="/security/findings/$findingId" params={{ findingId: finding.id }} className="font-medium text-primary hover:underline">
                  {finding.title}
                </Link>
                <div className="mt-1 text-xs text-muted-foreground">{finding.summary}</div>
              </TableCell>
              <TableCell><Badge variant={severityVariant(finding.severity)}>{finding.severity}</Badge></TableCell>
              <TableCell>{finding.validationStatus}</TableCell>
              <TableCell>{finding.state}</TableCell>
              <TableCell>{finding.filePath ? `${finding.filePath}${finding.line ? `:${finding.line}` : ''}` : '-'}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
