import { Network } from 'lucide-react'

import { Badge } from '@/components/ui/badge'
import { EmptyState } from '@/components/ui/empty-state'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useGatewayClasses } from '@/hooks/use-gateways'
import type { GatewayClass } from '@/schemas/gateway'

function capabilityChips(gatewayClass: GatewayClass): string[] {
  const capabilities = gatewayClass.spec.capabilities ?? {}
  return (
    [
      capabilities.inboundText && 'inbound text',
      capabilities.outboundText && 'outbound text',
      capabilities.threads && 'threads',
      capabilities.senderIdentity && 'sender identity',
      capabilities.explicitSessions && 'explicit sessions',
      capabilities.idempotentDelivery && 'idempotent delivery',
    ] as Array<string | false | undefined>
  ).filter((value): value is string => Boolean(value))
}

// Cluster-scoped adapter contracts. Read-only: classes are installed with the
// adapter deployment, not through this API.
export function GatewayClassesPanel() {
  const { data: classes, isLoading, error } = useGatewayClasses()

  if (isLoading) return <Skeleton className="h-40 w-full" />
  if (error) {
    return (
      <EmptyState
        icon={Network}
        headline="Could not load gateway classes"
        hint={error instanceof Error ? error.message : 'The request failed.'}
      />
    )
  }
  if (!classes || classes.length === 0) {
    return (
      <EmptyState
        icon={Network}
        headline="No gateway classes"
        hint="Gateway classes are cluster-scoped contracts installed alongside adapters."
      />
    )
  }

  return (
    <div className="rounded-md border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Category</TableHead>
            <TableHead>Contract</TableHead>
            <TableHead>Capabilities</TableHead>
            <TableHead>Accepted</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {classes.map((gatewayClass) => (
            <TableRow key={gatewayClass.metadata.name}>
              <TableCell className="font-medium">{gatewayClass.metadata.name}</TableCell>
              <TableCell><Badge variant="secondary">{gatewayClass.spec.category ?? '—'}</Badge></TableCell>
              <TableCell className="font-mono text-xs">{gatewayClass.spec.contractVersion ?? '—'}</TableCell>
              <TableCell>
                <div className="flex max-w-md flex-wrap gap-1">
                  {capabilityChips(gatewayClass).map((chip) => (
                    <Badge key={chip} variant="outline" className="text-xs">{chip}</Badge>
                  ))}
                </div>
              </TableCell>
              <TableCell>
                {gatewayClass.status?.accepted ? (
                  <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Accepted</Badge>
                ) : (
                  <Badge className="bg-status-pending-bg text-status-pending" variant="secondary">Pending</Badge>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
