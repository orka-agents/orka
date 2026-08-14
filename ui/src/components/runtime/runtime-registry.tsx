import { Activity, Boxes, ExternalLink, ShieldCheck } from 'lucide-react'
import { PageHeader } from '@/components/layout/page-header'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { ApiError } from '@/lib/api-client'
import { useAgentRuntimes, useRuntimePools } from '@/hooks/use-runtimes'
import { AgentRuntimeActions, RegisterRuntimeButton } from '@/components/runtime/agent-runtime-registration'
import { SubstratePoolsPanel } from '@/components/runtime/substrate-pools-panel'
import type { AgentRuntime, RuntimePool } from '@/schemas/runtime'

function stateClass(state?: string) {
  switch (state) {
    case 'Serving':
    case 'Accepting':
    case 'Ready':
      return 'bg-status-succeeded-bg text-status-succeeded'
    case 'Starting':
    case 'Draining':
    case 'Stopping':
    case 'Quiescent':
      return 'bg-status-running-bg text-status-running'
    case 'Degraded':
    case 'Ambiguous':
      return 'bg-status-failed-bg text-status-failed'
    default:
      return 'bg-muted text-muted-foreground'
  }
}

function compactDigest(value?: string) {
  if (!value) return '—'
  if (value.length <= 24) return value
  return `${value.slice(0, 15)}…${value.slice(-8)}`
}

function capacityText(current?: number, maximum?: number) {
  return `${current ?? 0} / ${maximum ?? 0}`
}

function CapacityMeter({ label, current = 0, maximum = 0 }: { label: string; current?: number; maximum?: number }) {
  const pct = maximum > 0 ? Math.min(100, Math.round((current / maximum) * 100)) : 0
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-xs">
        <span className="text-muted-foreground">{label}</span>
        <span className="font-mono">{capacityText(current, maximum)}</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted" aria-label={`${label} capacity`}>
        <div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}

function capabilityText(value?: boolean) {
  if (value === undefined) return 'Unknown'
  return value ? 'Yes' : 'No'
}

function RuntimePoolCard({ pool }: { pool: RuntimePool }) {
  const status = pool.status
  const capacity = status?.capacity
  const profile = pool.spec.runtime.profile
  return (
    <Card>
      <CardHeader className="space-y-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <CardTitle className="flex items-center gap-2">
              <Boxes className="h-4 w-4 text-primary" />
              {pool.metadata.name}
            </CardTitle>
            <p className="mt-1 text-xs text-muted-foreground">
              {pool.spec.trustDomain.namespace} · {pool.spec.trustDomain.identity}
            </p>
          </div>
          <div className="flex flex-wrap gap-2">
            <Badge variant="secondary" className={stateClass(status?.lifecycle)}>{status?.lifecycle ?? 'Unknown'}</Badge>
            <Badge variant="secondary" className={stateClass(status?.admissionState)}>{status?.admissionState ?? 'Closed'}</Badge>
          </div>
        </div>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <CapacityMeter label="Resident sessions" current={capacity?.residentSessions} maximum={capacity?.maxResidentSessions} />
          <CapacityMeter label="Running prompts" current={capacity?.runningPrompts} maximum={capacity?.maxRunningPrompts} />
        </div>
        <div className="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
          <div><span className="text-muted-foreground">Pods</span><div className="font-mono">{status?.currentReplicas ?? 0} / {status?.desiredReplicas ?? pool.spec.desiredReplicas ?? 0}</div></div>
          <div><span className="text-muted-foreground">Queued</span><div className="font-mono">{capacity?.queuedTasks ?? 0}</div></div>
          <div><span className="text-muted-foreground">Reserved</span><div className="font-mono">{capacity?.reservedSessions ?? 0}</div></div>
          <div><span className="text-muted-foreground">Finalizing</span><div className="font-mono">{capacity?.finalizingSessions ?? 0}</div></div>
        </div>
        <div className="grid gap-3 rounded-md border bg-muted/25 p-3 text-xs md:grid-cols-2">
          <div><span className="text-muted-foreground">ACP profile</span><div className="mt-0.5 font-mono">{profile.acpProfile}</div></div>
          <div><span className="text-muted-foreground">Workspace intent</span><div className="mt-0.5 font-medium capitalize">{profile.workspaceIntent}</div></div>
          <div><span className="text-muted-foreground">Provider / model</span><div className="mt-0.5">{profile.providerKind} · {profile.model}</div></div>
          <div><span className="text-muted-foreground">Profile digest</span><div className="mt-0.5 font-mono" title={profile.digest}>{compactDigest(profile.digest)}</div></div>
        </div>
        {status?.activeInstance && (
          <div className="space-y-1 border-t pt-3 text-xs">
            <div className="flex items-center gap-2 font-medium"><Activity className="h-3.5 w-3.5 text-primary" /> Active runtime instance</div>
            <div className="grid gap-1 text-muted-foreground md:grid-cols-2">
              <span>{status.activeInstance.podNamespace}/{status.activeInstance.podName}</span>
              <span className="font-mono" title={status.activeInstance.runtimeInstanceID}>{compactDigest(status.activeInstance.runtimeInstanceID)}</span>
            </div>
          </div>
        )}
        {status?.message && <p className="text-sm text-muted-foreground">{status.message}</p>}
      </CardContent>
    </Card>
  )
}

function AgentRuntimeCard({ runtime }: { runtime: AgentRuntime }) {
  const observed = runtime.status?.observedCapabilities

  if (runtime.spec.contractVersion === undefined) {
    return (
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div>
              <CardTitle className="flex items-center gap-2">
                <ExternalLink className="h-4 w-4 text-primary" />
                {runtime.metadata.name}
              </CardTitle>
              <p className="mt-1 text-xs text-muted-foreground">{runtime.spec.deployment.endpoint}</p>
            </div>
            <Badge variant="secondary" className={stateClass('Degraded')}>Not ready</Badge>
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="text-sm"><span className="text-muted-foreground">Contract</span><div className="font-mono">Unclassified</div></div>
          {runtime.status?.message && <p className="text-sm text-muted-foreground">{runtime.status.message}</p>}
        </CardContent>
      </Card>
    )
  }

  if (runtime.spec.contractVersion === 'orka.harness.v1') {
    const configured = runtime.spec.capabilities
    const hasObservedCapabilities = observed !== undefined
    const toolModes = hasObservedCapabilities
      ? observed.toolExecutionModes ?? []
      : configured?.toolExecutionModes ?? []
    const brokeredToolClasses = hasObservedCapabilities
      ? observed.brokeredToolClasses ?? []
      : configured?.brokeredToolClasses ?? []
    const supportsCancel = hasObservedCapabilities
      ? observed.supportsCancel ?? false
      : configured?.supportsCancel
    const supportsRuntimeSessions = hasObservedCapabilities
      ? observed.supportsRuntimeSessions ?? false
      : configured?.supportsRuntimeSessions
    const supportsContinuation = hasObservedCapabilities
      ? observed.supportsContinuation ?? false
      : configured?.supportsContinuation
    const supportsArtifacts = hasObservedCapabilities
      ? observed.supportsArtifacts ?? false
      : configured?.supportsArtifacts

    return (
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div>
              <CardTitle className="flex items-center gap-2">
                <ExternalLink className="h-4 w-4 text-primary" />
                {runtime.metadata.name}
              </CardTitle>
              <p className="mt-1 text-xs text-muted-foreground">{runtime.spec.deployment.endpoint}</p>
            </div>
            <Badge
              variant="secondary"
              className={runtime.status?.ready ? stateClass('Ready') : stateClass('Degraded')}
            >
              {runtime.status?.ready ? 'Ready' : 'Not ready'}
            </Badge>
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
            <div><span className="text-muted-foreground">Contract</span><div className="font-mono">{runtime.spec.contractVersion}</div></div>
            <div><span className="text-muted-foreground">Runtime name</span><div>{observed?.runtimeName ?? '—'}</div></div>
            <div><span className="text-muted-foreground">Runtime version</span><div>{observed?.runtimeVersion ?? '—'}</div></div>
            <div><span className="text-muted-foreground">Transport</span><div>{observed?.transport ?? '—'}</div></div>
          </div>
          <div className="grid gap-3 rounded-md border bg-muted/25 p-3 text-xs md:grid-cols-2 lg:grid-cols-3">
            <div><span className="text-muted-foreground">Tool modes</span><div className="mt-0.5">{toolModes.length > 0 ? toolModes.join(', ') : '—'}</div></div>
            <div><span className="text-muted-foreground">Brokered tool classes</span><div className="mt-0.5">{brokeredToolClasses.length > 0 ? brokeredToolClasses.join(', ') : '—'}</div></div>
            <div><span className="text-muted-foreground">Concurrent turns</span><div className="mt-0.5 font-mono">{observed?.maxConcurrentTurns ?? '—'}</div></div>
            <div><span className="text-muted-foreground">Maximum turn</span><div className="mt-0.5 font-mono">{observed?.maxTurnSeconds === undefined ? '—' : `${observed.maxTurnSeconds}s`}</div></div>
            <div><span className="text-muted-foreground">Maximum output</span><div className="mt-0.5 font-mono">{observed?.maxOutputBytes === undefined ? '—' : `${observed.maxOutputBytes} bytes`}</div></div>
            <div><span className="text-muted-foreground">Provider</span><div className="mt-0.5">{observed?.providerKind ?? '—'}</div></div>
          </div>
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <Activity className="h-4 w-4 text-primary" />
            <Badge variant="outline">Cancel: {capabilityText(supportsCancel)}</Badge>
            <Badge variant="outline">Runtime sessions: {capabilityText(supportsRuntimeSessions)}</Badge>
            <Badge variant="outline">Continuation: {capabilityText(supportsContinuation)}</Badge>
            <Badge variant="outline">Artifacts: {capabilityText(supportsArtifacts)}</Badge>
            {hasObservedCapabilities && <Badge variant="outline">Suspend: {capabilityText(observed.supportsSuspend ?? false)}</Badge>}
            {hasObservedCapabilities && <Badge variant="outline">Workspace snapshot: {capabilityText(observed.supportsWorkspaceSnapshot ?? false)}</Badge>}
          </div>
          {runtime.status?.lastValidated && (
            <p className="text-xs text-muted-foreground">Validated {new Date(runtime.status.lastValidated).toLocaleString()}</p>
          )}
          {runtime.status?.message && <p className="text-sm text-muted-foreground">{runtime.status.message}</p>}
        </CardContent>
      </Card>
    )
  }

  const configured = runtime.spec.capabilities
  const profile = observed?.runtimeProfileDigest ? {
    digest: observed.runtimeProfileDigest,
    adapter: observed.adapterName,
    provider: observed.providerKind,
    model: observed.model,
  } : {
    digest: configured.profile.digest,
    adapter: configured.profile.adapterName,
    provider: configured.profile.providerKind,
    model: configured.profile.model,
  }
  const governance = observed?.workspaceGovernance ?? configured.workspaceGovernance
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <CardTitle className="flex items-center gap-2">
              <ExternalLink className="h-4 w-4 text-primary" />
              {runtime.metadata.name}
            </CardTitle>
            <p className="mt-1 text-xs text-muted-foreground">{runtime.spec.deployment.endpoint}</p>
          </div>
          <Badge
            variant="secondary"
            className={runtime.status?.ready ? stateClass('Ready') : stateClass('Degraded')}
          >
            {runtime.status?.ready ? 'Ready' : 'Not ready'}
          </Badge>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
          <div><span className="text-muted-foreground">Contract</span><div className="font-mono">{runtime.spec.contractVersion}</div></div>
          <div><span className="text-muted-foreground">ACP profile</span><div className="font-mono">{configured.profile.acpProfile}</div></div>
          <div><span className="text-muted-foreground">Adapter</span><div>{profile.adapter ?? '—'}</div></div>
          <div><span className="text-muted-foreground">Provider / model</span><div>{profile.provider ?? '—'} · {profile.model ?? '—'}</div></div>
        </div>
        <div className="grid gap-3 rounded-md border bg-muted/25 p-3 text-xs md:grid-cols-2">
          <div><span className="text-muted-foreground">Runtime instance</span><div className="mt-0.5 font-mono" title={observed?.runtimeInstanceID ?? configured.runtimeInstanceID}>{compactDigest(observed?.runtimeInstanceID ?? configured.runtimeInstanceID)}</div></div>
          <div><span className="text-muted-foreground">Profile digest</span><div className="mt-0.5 font-mono" title={profile.digest}>{compactDigest(profile.digest)}</div></div>
          <div><span className="text-muted-foreground">Capacity</span><div className="mt-0.5">{configured.limits.maxResidentSessions} sessions · {configured.limits.maxConcurrentPrompts} prompts</div></div>
          <div><span className="text-muted-foreground">Workspace intent</span><div className="mt-0.5 font-medium capitalize">{configured.profile.workspaceIntent}</div></div>
        </div>
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <ShieldCheck className="h-4 w-4 text-primary" />
          <Badge variant="outline">{governance.mode ?? configured.workspaceGovernance.mode}</Badge>
          {configured.supportsDrain && <Badge variant="outline">Drain</Badge>}
          {configured.supportsPublicationFinalization && <Badge variant="outline">Publication finalization</Badge>}
          {governance.exactInstanceFencing && <Badge variant="outline">Exact-instance fencing</Badge>}
          {governance.cancellationSettlement && <Badge variant="outline">Cancellation settlement</Badge>}
          {governance.noDirectSCMPublication && <Badge variant="outline">No direct SCM publication</Badge>}
        </div>
        {runtime.status?.lastValidated && (
          <p className="text-xs text-muted-foreground">Validated {new Date(runtime.status.lastValidated).toLocaleString()}</p>
        )}
        {runtime.status?.message && <p className="text-sm text-muted-foreground">{runtime.status.message}</p>}
      </CardContent>
    </Card>
  )
}

function RuntimeAPIError({ error, resource }: { error: unknown; resource: string }) {
  const unavailable = error instanceof ApiError && (error.status === 404 || error.status === 501)
  return (
    <Card>
      <CardContent className="pt-6">
        <EmptyState
          icon={Activity}
          headline={unavailable ? `${resource} API unavailable` : `Could not load ${resource.toLowerCase()}`}
          hint={unavailable
            ? 'This controller build does not expose the ACP runtime registry REST endpoints yet.'
            : error instanceof Error ? error.message : 'The runtime registry request failed.'}
        />
      </CardContent>
    </Card>
  )
}

export function RuntimeRegistry() {
  const pools = useRuntimePools()
  const runtimes = useAgentRuntimes()

  return (
    <div className="space-y-4">
      <PageHeader
        title="Runtime fabric"
        description="ACP runtime capacity, admission, exact-instance fencing, and external runtime registrations"
      />
      <Tabs defaultValue="pools">
        <TabsList>
          <TabsTrigger value="pools">Runtime pools</TabsTrigger>
          <TabsTrigger value="external">External runtimes</TabsTrigger>
          <TabsTrigger value="substrate">Substrate pools</TabsTrigger>
        </TabsList>
        <TabsContent value="pools" className="space-y-4">
          {pools.isLoading && <><Skeleton className="h-56 w-full" /><Skeleton className="h-56 w-full" /></>}
          {pools.error && <RuntimeAPIError error={pools.error} resource="RuntimePool" />}
          {!pools.isLoading && !pools.error && (pools.data?.items.length ?? 0) === 0 && (
            <EmptyState icon={Boxes} headline="No runtime pools" hint="No ACP RuntimePool is registered in this namespace. Pools are controller-owned and appear when agent Tasks dispatch." />
          )}
          {pools.data?.items.map((pool) => <RuntimePoolCard key={pool.metadata.uid ?? pool.metadata.name} pool={pool} />)}
        </TabsContent>
        <TabsContent value="external" className="space-y-4">
          <div className="flex items-center justify-between gap-3">
            <p className="text-sm text-muted-foreground">
              Registrations validate and probe conformance. Task dispatch to external runtimes is fail-closed until the v2 dispatcher lands.
            </p>
            <RegisterRuntimeButton />
          </div>
          {runtimes.isLoading && <><Skeleton className="h-56 w-full" /><Skeleton className="h-56 w-full" /></>}
          {runtimes.error && <RuntimeAPIError error={runtimes.error} resource="AgentRuntime" />}
          {!runtimes.isLoading && !runtimes.error && (runtimes.data?.items.length ?? 0) === 0 && (
            <EmptyState icon={ExternalLink} headline="No external runtimes" hint="No AgentRuntime is registered in this namespace." />
          )}
          {runtimes.data?.items.map((runtime) => (
            <div key={runtime.metadata.uid ?? runtime.metadata.name} className="space-y-1.5">
              <AgentRuntimeCard runtime={runtime} />
              <AgentRuntimeActions runtime={runtime} />
            </div>
          ))}
        </TabsContent>
        <TabsContent value="substrate" className="space-y-4">
          <SubstratePoolsPanel />
        </TabsContent>
      </Tabs>
    </div>
  )
}
