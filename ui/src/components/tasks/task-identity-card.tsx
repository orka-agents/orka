import { Fingerprint } from 'lucide-react'

import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import type { Task } from '@/schemas/task'

function Row({ label, value, mono = true }: { label: string; value?: string | null; mono?: boolean }) {
  if (!value) return null
  return (
    <div className="grid grid-cols-[8rem_1fr] gap-2 py-1 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className={mono ? 'break-all font-mono text-xs leading-5' : ''}>{value}</span>
    </div>
  )
}

// Server-stamped requester identity and transaction-token provenance.
// Both fields are immutable and rejected on client writes, so everything
// here is verified audit data.
export function TaskIdentityCard({ task }: { task: Task }) {
  const requestedBy = task.spec.requestedBy
  const transaction = task.spec.transaction
  if (!requestedBy && !transaction) return null

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Fingerprint className="h-4 w-4" />
          Requested by
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        {requestedBy && (
          <div>
            <Row label="Username" value={requestedBy.username} />
            <Row label="Subject" value={requestedBy.subject} />
            <Row label="Email" value={requestedBy.email} />
            <Row label="Issuer" value={requestedBy.issuer} />
            <Row label="Groups" value={requestedBy.groups?.join(', ')} mono={false} />
            <Row label="Roles" value={requestedBy.roles?.join(', ')} mono={false} />
          </div>
        )}
        {transaction && (
          <div className="border-t pt-3">
            <div className="mb-1.5 flex items-center gap-2">
              <p className="text-sm font-medium">Transaction</p>
              {transaction.profile && <Badge variant="secondary">{transaction.profile}</Badge>}
            </div>
            <Row label="ID" value={transaction.id} />
            <Row label="Subject" value={transaction.subject} />
            <Row label="Workload" value={transaction.requestingWorkload} />
            <Row label="Audience" value={transaction.audience?.join(', ')} />
            <Row label="Scopes" value={transaction.scopes?.join(' ') ?? transaction.scope} />
            <Row label="Context digest" value={transaction.contextDigest} />
          </div>
        )}
      </CardContent>
    </Card>
  )
}
