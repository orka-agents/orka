import { useState } from 'react'
import { Plus } from 'lucide-react'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { ManifestEditor } from '@/components/ui/manifest-editor'
import {
  useCreateAgentRuntime,
  useDeleteAgentRuntime,
  useUpdateAgentRuntime,
} from '@/hooks/use-runtimes'
import { useUIStore } from '@/stores/ui'
import type { AgentRuntime } from '@/schemas/runtime'

// Starter manifest for a v2 external runtime registration. Server-side
// validation is authoritative; this just seeds the editor.
const registrationTemplate = {
  metadata: { name: 'my-runtime' },
  spec: {
    contractVersion: 'orka.harness.v2',
    deployment: { mode: 'external-endpoint', endpoint: 'https://runtime.example.internal' },
    clientAuth: {
      controllerBearerTokenSecretRef: { name: 'runtime-controller-token', key: 'token' },
      operationCapabilitySecretRef: { name: 'runtime-capability-secret', key: 'secret' },
    },
    capabilities: {
      runtimeInstanceID: 'instance-1',
      profile: {
        digest: '',
        acpProfile: 'acp.v1',
        providerKind: 'claude',
        model: '',
        workspaceIntent: 'read',
      },
      limits: { maxResidentSessions: 10, maxConcurrentPrompts: 4 },
      workspaceGovernance: { mode: 'strict-governed' },
    },
  },
}

export function RegisterRuntimeButton() {
  const namespace = useUIStore((s) => s.namespace)
  const createRuntime = useCreateAgentRuntime()
  const [open, setOpen] = useState(false)

  return (
    <>
      <Button onClick={() => setOpen(true)}>
        <Plus className="mr-2 h-4 w-4" />
        Register runtime
      </Button>
      <ManifestEditor
        open={open}
        onOpenChange={setOpen}
        title="Register external runtime"
        description="Submit a full AgentRuntime manifest. Registration and conformance run server-side; Task dispatch to external runtimes stays fail-closed until the v2 dispatcher is wired."
        initialValue={registrationTemplate}
        submitLabel="Register"
        pending={createRuntime.isPending}
        onSubmit={async (manifest) => {
          const metadata = (manifest.metadata ?? {}) as Record<string, unknown>
          const body = {
            ...manifest,
            metadata: { namespace, ...metadata },
          }
          await createRuntime.mutateAsync(body)
          const name = typeof metadata.name === 'string' ? metadata.name : 'runtime'
          toast.success(`Runtime ${name} registered`)
          setOpen(false)
        }}
      />
    </>
  )
}

export function AgentRuntimeActions({ runtime }: { runtime: AgentRuntime }) {
  const updateRuntime = useUpdateAgentRuntime()
  const deleteRuntime = useDeleteAgentRuntime()
  const [editing, setEditing] = useState(false)
  const [confirming, setConfirming] = useState(false)

  const handleDelete = async () => {
    if (!confirming) {
      setConfirming(true)
      return
    }
    try {
      await deleteRuntime.mutateAsync(runtime.metadata.name)
      toast.success(`Runtime ${runtime.metadata.name} removed`)
    } catch (error) {
      toast.error(`Failed to remove runtime: ${error instanceof Error ? error.message : 'unknown error'}`)
    } finally {
      setConfirming(false)
    }
  }

  return (
    <div className="flex items-center justify-end gap-1.5">
      <Button variant="ghost" size="sm" onClick={() => setEditing(true)}>Edit spec</Button>
      <Button
        variant={confirming ? 'destructive' : 'ghost'}
        size="sm"
        onClick={handleDelete}
        disabled={deleteRuntime.isPending}
      >
        {confirming ? 'Confirm remove' : 'Remove'}
      </Button>
      <ManifestEditor
        open={editing}
        onOpenChange={setEditing}
        title={`Edit ${runtime.metadata.name}`}
        description="Only .spec is applied on update. contractVersion is immutable."
        initialValue={{ spec: runtime.spec }}
        submitLabel="Save changes"
        pending={updateRuntime.isPending}
        onSubmit={async (manifest) => {
          await updateRuntime.mutateAsync({ name: runtime.metadata.name, body: manifest })
          toast.success(`Runtime ${runtime.metadata.name} updated`)
          setEditing(false)
        }}
      />
    </div>
  )
}
