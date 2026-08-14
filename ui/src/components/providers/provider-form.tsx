import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { PageHeader } from '@/components/layout/page-header'
import { useCreateProvider, useUpdateProvider } from '@/hooks/use-providers'
import { useSecretNames } from '@/hooks/use-secrets'
import { useUIStore } from '@/stores/ui'
import type { Provider, ProviderSpec, ProviderType } from '@/schemas/provider'

interface ProviderFormProps {
  /** When set, the form edits this provider instead of creating a new one. */
  initial?: Provider
}

export function ProviderForm({ initial }: ProviderFormProps) {
  const navigate = useNavigate()
  const namespace = useUIStore((s) => s.namespace)
  const createProvider = useCreateProvider()
  const updateProvider = useUpdateProvider()
  const { data: secrets } = useSecretNames()

  const editing = Boolean(initial)
  const [name, setName] = useState(initial?.metadata.name ?? '')
  const [type, setType] = useState<ProviderType>((initial?.spec.type as ProviderType) ?? 'anthropic')
  const [secretName, setSecretName] = useState(initial?.spec.secretRef?.name ?? '')
  const [secretKey, setSecretKey] = useState(initial?.spec.secretRef?.key ?? '')
  const [baseURL, setBaseURL] = useState(initial?.spec.baseURL ?? '')
  const [defaultModel, setDefaultModel] = useState(initial?.spec.defaultModel ?? '')
  const [rpm, setRpm] = useState(initial?.spec.rateLimit?.requestsPerMinute?.toString() ?? '')
  const [tpm, setTpm] = useState(initial?.spec.rateLimit?.tokensPerMinute?.toString() ?? '')
  const [azureDeployment, setAzureDeployment] = useState(initial?.spec.azure?.deploymentName ?? '')
  const [azureApiVersion, setAzureApiVersion] = useState(initial?.spec.azure?.apiVersion ?? '')

  const pending = createProvider.isPending || updateProvider.isPending

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!name.trim()) {
      toast.error('Name is required')
      return
    }
    if (!secretName.trim()) {
      toast.error('Credentials Secret is required')
      return
    }

    const spec: ProviderSpec = {
      type,
      secretRef: { name: secretName.trim(), ...(secretKey.trim() ? { key: secretKey.trim() } : {}) },
      ...(baseURL.trim() ? { baseURL: baseURL.trim() } : {}),
      ...(defaultModel.trim() ? { defaultModel: defaultModel.trim() } : {}),
      ...(rpm || tpm
        ? {
            rateLimit: {
              ...(rpm ? { requestsPerMinute: Number(rpm) } : {}),
              ...(tpm ? { tokensPerMinute: Number(tpm) } : {}),
            },
          }
        : {}),
      ...(type === 'azure-openai' && (azureDeployment || azureApiVersion)
        ? {
            azure: {
              ...(azureDeployment ? { deploymentName: azureDeployment } : {}),
              ...(azureApiVersion ? { apiVersion: azureApiVersion } : {}),
            },
          }
        : {}),
    }

    try {
      if (editing && initial) {
        await updateProvider.mutateAsync({ name: initial.metadata.name, spec })
        toast.success(`Provider ${initial.metadata.name} updated`)
        navigate({ to: '/providers/$providerName', params: { providerName: initial.metadata.name } })
      } else {
        await createProvider.mutateAsync({ name: name.trim(), namespace, spec })
        toast.success(`Provider ${name.trim()} created`)
        navigate({ to: '/providers' })
      }
    } catch (error) {
      toast.error(`Failed to ${editing ? 'update' : 'create'} provider: ${error instanceof Error ? error.message : 'unknown error'}`)
    }
  }

  return (
    <div className="mx-auto max-w-2xl space-y-4">
      <PageHeader
        eyebrow="Providers"
        title={editing ? `Edit ${initial?.metadata.name}` : 'New provider'}
        description={editing
          ? 'Update routing and limits. The stored base URL is preserved server-side.'
          : 'Register an LLM provider backed by a credentials Secret.'}
      />
      <form onSubmit={handleSubmit}>
        <Card>
          <CardHeader>
            <CardTitle>Provider</CardTitle>
            <CardDescription>Credentials stay in the referenced Secret — only its name is stored here.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {!editing && (
              <div className="space-y-2">
                <label htmlFor="provider-name" className="text-sm font-medium">Name</label>
                <Input
                  id="provider-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="anthropic"
                />
              </div>
            )}
            <div className="space-y-2">
              <label className="text-sm font-medium">Type</label>
              <Select value={type} onValueChange={(v) => setType(v as ProviderType)}>
                <SelectTrigger aria-label="Provider type"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="anthropic">anthropic</SelectItem>
                  <SelectItem value="openai">openai</SelectItem>
                  <SelectItem value="azure-openai">azure-openai</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Credentials Secret</label>
                <Select value={secretName} onValueChange={setSecretName}>
                  <SelectTrigger aria-label="Credentials Secret"><SelectValue placeholder="Select a Secret" /></SelectTrigger>
                  <SelectContent>
                    {(secrets?.items ?? []).map((secret) => (
                      <SelectItem key={secret.name} value={secret.name}>{secret.name}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <label htmlFor="provider-secret-key" className="text-sm font-medium">Secret key (optional)</label>
                <Input
                  id="provider-secret-key"
                  value={secretKey}
                  onChange={(e) => setSecretKey(e.target.value)}
                  placeholder="api-key"
                />
              </div>
            </div>
            <div className="space-y-2">
              <label htmlFor="provider-base-url" className="text-sm font-medium">Base URL (optional)</label>
              <Input
                id="provider-base-url"
                value={baseURL}
                onChange={(e) => setBaseURL(e.target.value)}
                placeholder="https://proxy.internal/v1"
                disabled={editing}
              />
              {editing && (
                <p className="text-xs text-muted-foreground">
                  The server preserves the existing base URL on update.
                </p>
              )}
            </div>
            <div className="space-y-2">
              <label htmlFor="provider-default-model" className="text-sm font-medium">Default model (optional)</label>
              <Input
                id="provider-default-model"
                value={defaultModel}
                onChange={(e) => setDefaultModel(e.target.value)}
                placeholder="claude-sonnet-4-20250514"
              />
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2">
                <label htmlFor="provider-rpm" className="text-sm font-medium">Requests / minute (optional)</label>
                <Input id="provider-rpm" type="number" min="0" value={rpm} onChange={(e) => setRpm(e.target.value)} />
              </div>
              <div className="space-y-2">
                <label htmlFor="provider-tpm" className="text-sm font-medium">Tokens / minute (optional)</label>
                <Input id="provider-tpm" type="number" min="0" value={tpm} onChange={(e) => setTpm(e.target.value)} />
              </div>
            </div>
            {type === 'azure-openai' && (
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-2">
                  <label htmlFor="provider-azure-deployment" className="text-sm font-medium">Azure deployment</label>
                  <Input
                    id="provider-azure-deployment"
                    value={azureDeployment}
                    onChange={(e) => setAzureDeployment(e.target.value)}
                  />
                </div>
                <div className="space-y-2">
                  <label htmlFor="provider-azure-api-version" className="text-sm font-medium">Azure API version</label>
                  <Input
                    id="provider-azure-api-version"
                    value={azureApiVersion}
                    onChange={(e) => setAzureApiVersion(e.target.value)}
                  />
                </div>
              </div>
            )}
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => navigate({ to: '/providers' })}>
                Cancel
              </Button>
              <Button type="submit" disabled={pending}>
                {pending ? 'Saving…' : editing ? 'Save changes' : 'Create provider'}
              </Button>
            </div>
          </CardContent>
        </Card>
      </form>
    </div>
  )
}
