import { useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { Pencil, Trash2 } from 'lucide-react'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { useDeleteProvider, useProvider } from '@/hooks/use-providers'

function Field({ label, value, mono }: { label: string; value?: string | number | null; mono?: boolean }) {
  return (
    <div className="grid grid-cols-[10rem_1fr] gap-2 py-1.5 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className={mono ? 'break-all font-mono text-xs leading-5' : ''}>{value ?? '-'}</span>
    </div>
  )
}

export function ProviderDetail({ providerName }: { providerName: string }) {
  const navigate = useNavigate()
  const { data: provider, isLoading, isError } = useProvider(providerName)
  const deleteProvider = useDeleteProvider()
  const [confirming, setConfirming] = useState(false)

  const handleDelete = async () => {
    if (!confirming) {
      setConfirming(true)
      return
    }
    try {
      await deleteProvider.mutateAsync(providerName)
      toast.success(`Provider ${providerName} deleted`)
      navigate({ to: '/providers' })
    } catch (error) {
      toast.error(`Failed to delete provider: ${error instanceof Error ? error.message : 'unknown error'}`)
      setConfirming(false)
    }
  }

  if (isLoading) {
    return <Skeleton className="h-64 w-full" />
  }
  if (isError || !provider) {
    return (
      <div className="space-y-4">
        <PageHeader eyebrow="Providers" title={providerName} description="Provider not found in this namespace." />
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <PageHeader
        eyebrow="Providers"
        title={provider.metadata.name}
        description={provider.status?.message}
        action={
          <>
            <Button asChild variant="outline">
              <Link to="/providers/$providerName/edit" params={{ providerName }}>
                <Pencil className="mr-2 h-4 w-4" />
                Edit
              </Link>
            </Button>
            <Button variant={confirming ? 'destructive' : 'outline'} onClick={handleDelete} disabled={deleteProvider.isPending}>
              <Trash2 className="mr-2 h-4 w-4" />
              {confirming ? 'Confirm delete' : 'Delete'}
            </Button>
          </>
        }
      />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center justify-between">
              Configuration
              {provider.status?.ready ? (
                <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Ready</Badge>
              ) : (
                <Badge className="bg-status-pending-bg text-status-pending" variant="secondary">Not ready</Badge>
              )}
            </CardTitle>
          </CardHeader>
          <CardContent>
            <Field label="Type" value={provider.spec.type} />
            <Field label="Credentials Secret" value={provider.spec.secretRef?.name} mono />
            <Field label="Secret key" value={provider.spec.secretRef?.key} mono />
            <Field label="Base URL" value={provider.spec.baseURL} mono />
            <Field label="Default model" value={provider.spec.defaultModel} mono />
            <Field label="Requests / minute" value={provider.spec.rateLimit?.requestsPerMinute} />
            <Field label="Tokens / minute" value={provider.spec.rateLimit?.tokensPerMinute} />
            {provider.spec.type === 'azure-openai' && (
              <>
                <Field label="Azure deployment" value={provider.spec.azure?.deploymentName} mono />
                <Field label="Azure API version" value={provider.spec.azure?.apiVersion} mono />
              </>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Status</CardTitle>
          </CardHeader>
          <CardContent>
            <Field label="Last validated" value={provider.status?.lastValidated} mono />
            <Field label="Message" value={provider.status?.message} />
            {(provider.status?.conditions ?? []).map((condition) => (
              <Field
                key={condition.type}
                label={condition.type}
                value={`${condition.status}${condition.reason ? ` (${condition.reason})` : ''}`}
              />
            ))}
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
