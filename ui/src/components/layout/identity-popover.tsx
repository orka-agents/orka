import { UserRound } from 'lucide-react'

import { useWhoAmI } from '@/hooks/use-whoami'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Separator } from '@/components/ui/separator'

function Row({ label, value }: { label: string; value?: string | null }) {
  if (!value) return null
  return (
    <div className="grid grid-cols-[5.5rem_1fr] gap-2 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="break-all font-mono text-xs leading-5">{value}</span>
    </div>
  )
}

// Shows the verified identity the API server resolved from the current
// credential — the same view GET /auth/whoami returns.
export function IdentityPopover() {
  const { data: whoami, isError } = useWhoAmI()

  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="icon" aria-label="Account identity">
          <UserRound className="h-4 w-4" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80">
        {isError && (
          <p className="text-sm text-muted-foreground">
            Identity lookup failed. The token may lack access to /auth/whoami.
          </p>
        )}
        {!isError && !whoami && <p className="text-sm text-muted-foreground">Loading identity…</p>}
        {whoami && (
          <div className="space-y-3">
            <div className="flex items-center justify-between gap-2">
              <p className="text-sm font-medium">Signed in</p>
              {whoami.authType && <Badge variant="secondary">{whoami.authType}</Badge>}
            </div>
            <div className="space-y-1.5">
              <Row label="Username" value={whoami.username} />
              <Row label="Subject" value={whoami.subject} />
              <Row label="Email" value={whoami.email} />
              <Row label="Issuer" value={whoami.issuer} />
              <Row label="Namespace" value={whoami.namespace} />
              <Row label="Groups" value={whoami.groups?.length ? whoami.groups.join(', ') : undefined} />
              <Row label="Roles" value={whoami.roles?.length ? whoami.roles.join(', ') : undefined} />
            </div>
            {whoami.transaction && (
              <>
                <Separator />
                <div className="space-y-1.5">
                  <p className="text-sm font-medium">Transaction token</p>
                  <Row label="Profile" value={whoami.transaction.profile} />
                  <Row label="ID" value={whoami.transaction.id} />
                  <Row label="Workload" value={whoami.transaction.requestingWorkload} />
                  <Row
                    label="Scopes"
                    value={
                      whoami.transaction.scopes?.length
                        ? whoami.transaction.scopes.join(' ')
                        : whoami.transaction.scope
                    }
                  />
                </div>
              </>
            )}
          </div>
        )}
      </PopoverContent>
    </Popover>
  )
}
