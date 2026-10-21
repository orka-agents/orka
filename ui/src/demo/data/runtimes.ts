import type { AgentRuntime, RuntimePool } from '@/schemas/runtime'
import { ago, DEMO_NAMESPACE, digest, uid } from '../shared'
import { AGENTS } from '../world'

// ACP runtime fabric: controller-owned, digest-pinned RuntimePools for the
// built-in coding-agent runtimes, plus external AgentRuntime registrations.

const RUNTIME_NAMESPACE = 'orka-runtimes'
const sha = (seed: string) => `sha256:${digest(seed, 64)}`

const ADAPTERS: Record<string, string[]> = {
  claude: ['claude-agent-acp', 'claude-code-linux-amd64', 'claude-code-linux-arm64'],
  codex: ['codex-acp', 'codex-acp-orka-patch', 'codex-cli-linux-amd64', 'codex-cli-linux-arm64'],
  copilot: ['copilot-cli-linux-amd64', 'copilot-cli-linux-arm64'],
  opencode: ['opencode-cli-linux-amd64', 'opencode-cli-linux-arm64'],
}

interface PoolSeed {
  runtime: 'claude' | 'codex' | 'copilot' | 'opencode'
  model: string
  intent: 'read' | 'write'
  /** distinguishes an older profile of the same runtime */
  revision?: string
  createdAgo: number
  lifecycle: NonNullable<RuntimePool['status']>['lifecycle']
  admission: NonNullable<RuntimePool['status']>['admissionState']
  replicas: number
  maxResident: number
  maxPrompts: number
  resident: number
  running: number
  queued?: number
  reserved?: number
  finalizing?: number
  modelLimits?: { context: number; output: number }
  message?: string
}

const poolSeeds: PoolSeed[] = [
  { runtime: 'claude', model: AGENTS.reviewer.model, intent: 'read', createdAgo: 3 * 24 * 60, lifecycle: 'Serving', admission: 'Accepting', replicas: 1, maxResident: 8, maxPrompts: 4, resident: 2, running: 1, queued: 1, reserved: 1 },
  { runtime: 'claude', model: AGENTS.reviewer.model, intent: 'read', revision: 'previous', createdAgo: 9 * 24 * 60, lifecycle: 'Draining', admission: 'Draining', replicas: 1, maxResident: 8, maxPrompts: 4, resident: 1, running: 0, finalizing: 1, message: 'Runtime image superseded; draining 1 resident session before scale-down.' },
  { runtime: 'codex', model: AGENTS.fixer.model, intent: 'write', createdAgo: 6 * 24 * 60, lifecycle: 'Serving', admission: 'Accepting', replicas: 1, maxResident: 4, maxPrompts: 2, resident: 1, running: 1 },
  { runtime: 'copilot', model: AGENTS.docs.model, intent: 'write', createdAgo: 2 * 24 * 60, lifecycle: 'Serving', admission: 'Accepting', replicas: 1, maxResident: 4, maxPrompts: 2, resident: 1, running: 1 },
  { runtime: 'opencode', model: `openai/${AGENTS.deps.model}`, intent: 'write', createdAgo: 12 * 24 * 60, lifecycle: 'Stopped', admission: 'Closed', replicas: 0, maxResident: 4, maxPrompts: 2, resident: 0, running: 0, modelLimits: { context: 1047576, output: 32768 }, message: 'Scaled to zero after 30m without sessions; the next Task cold-starts a runtime Pod.' },
]

function runtimePool(seed: PoolSeed, index: number): RuntimePool {
  const key = `${seed.runtime}:${seed.model}:${seed.intent}:${seed.revision ?? 'current'}`
  const profileDigest = sha(`profile:${key}`)
  const name = `acp-${seed.runtime}-${digest(`pool:${key}`, 16)}`
  const image = `ghcr.io/orka-agents/acp-${seed.runtime}-runtime@${sha(`image:${key}`)}`
  const generation = seed.revision ? 3 : 1
  const podName = `${name}-${digest(`rs:${key}`, 9)}-${digest(`pod:${key}`, 5)}`
  const serving = seed.replicas > 0
  return {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'RuntimePool',
    metadata: {
      name,
      namespace: DEMO_NAMESPACE,
      uid: uid(`runtimepool:${key}`),
      creationTimestamp: ago(seed.createdAgo),
      labels: { 'orka.ai/runtime': seed.runtime, 'orka.ai/managed-by': 'orka-controller' },
    },
    spec: {
      trustDomain: { namespace: DEMO_NAMESPACE, identity: `namespace:${DEMO_NAMESPACE}` },
      runtimeNamespace: RUNTIME_NAMESPACE,
      runtime: {
        image,
        profile: {
          protocolVersion: 'orka.harness.v2',
          digest: profileDigest,
          digestSchemaVersion: '1',
          acpProfile: 'acp.v1',
          adapterDigests: Object.fromEntries([...ADAPTERS[seed.runtime], 'acp-schema'].map((adapter) => [adapter, sha(`${adapter}:${key}`)])),
          providerKind: seed.runtime,
          model: seed.model,
          ...(seed.modelLimits && { modelLimits: seed.modelLimits }),
          agentConfigurationDigest: sha(`agent-config:${key}`),
          toolPolicyDigest: sha(`tool-policy:${key}`),
          approvalPolicyDigest: sha(`approval-policy:${key}`),
          mcpConfigurationDigest: sha(`mcp-config:${key}`),
          workspaceIntent: seed.intent,
          proxyCredentialRole: 'provider-inference',
          proxyCredentialScope: `model:${seed.model}`,
          resourceClass: 'standard',
        },
      },
      desiredReplicas: seed.replicas,
      capacity: { maxResidentSessions: seed.maxResident, maxRunningPrompts: seed.maxPrompts },
      coldStartTimeoutSeconds: 180,
    },
    status: {
      observedGeneration: generation,
      controllerEpoch: 14,
      desiredReplicas: seed.replicas,
      currentReplicas: seed.replicas,
      lifecycle: seed.lifecycle,
      admissionState: seed.admission,
      ...(serving && {
        activeInstance: {
          podNamespace: RUNTIME_NAMESPACE,
          podName,
          podAddress: `10.244.${2 + index}.${17 + index * 9}`,
          podUID: uid(`pod:${key}`),
          bootID: uid(`boot:${key}`),
          runtimeInstanceID: `rti-${digest(`instance:${key}`, 24)}`,
          controllerEpoch: 14,
          protocolVersion: 'orka.harness.v2' as const,
          profileDigest,
          profileDigestSchemaVersion: '1',
          lastObservedTime: ago(0.2),
        },
      }),
      capacity: {
        maxResidentSessions: seed.maxResident,
        maxRunningPrompts: seed.maxPrompts,
        residentSessions: seed.resident,
        runningPrompts: seed.running,
        queuedTasks: seed.queued ?? 0,
        reservedSessions: seed.reserved ?? 0,
        pendingPermissions: 0,
        finalizingSessions: seed.finalizing ?? 0,
        liveDescendants: 0,
      },
      ...(seed.message && { message: seed.message }),
      conditions: [
        { type: 'Ready', status: serving ? 'True' : 'False', reason: serving ? 'Serving' : 'ScaledToZero', lastTransitionTime: ago(serving ? seed.createdAgo - 2 : 31) },
        { type: 'Admitting', status: seed.admission === 'Accepting' ? 'True' : 'False', reason: seed.admission ?? 'Closed', lastTransitionTime: ago(serving ? seed.createdAgo - 2 : 31) },
      ],
    },
  }
}

export const runtimePools: RuntimePool[] = poolSeeds.map(runtimePool)

/** The current (non-draining) pool for a built-in runtime, which task status names. */
export function currentRuntimePool(runtime: string): RuntimePool {
  const index = poolSeeds.findIndex((seed) => seed.runtime === runtime && !seed.revision)
  return runtimePools[index === -1 ? 0 : index]
}

const limits = {
  maxResidentSessions: 6,
  maxConcurrentPrompts: 3,
  maxRequestBytes: 1048576,
  maxEventLineBytes: 262144,
  maxTerminalResultBytes: 1048576,
  maxBufferedEvents: 4096,
  maxUpdateEventsPerSecond: 50,
  minPromptLeaseMillis: 30000,
  maxPromptLeaseMillis: 3600000,
  maxPendingPermissions: 8,
  maxWorkspaceDeltaBytes: 52428800,
}

const gooseProfile = {
  digest: sha('goose:profile'),
  digestSchemaVersion: 1,
  acpProfile: 'acp.v1',
  adapterName: 'goose-acp',
  adapterDigest: sha('goose:adapter'),
  providerKind: 'goose',
  model: 'claude-sonnet-4-5',
  agentConfigurationDigest: sha('goose:agent-config'),
  toolPolicyDigest: sha('goose:tool-policy'),
  approvalPolicyDigest: sha('goose:approval-policy'),
  mcpConfigurationDigest: sha('goose:mcp-config'),
  workspaceIntent: 'write' as const,
  proxyCredentialRole: 'provider-inference',
  proxyCredentialScope: 'model:claude-sonnet-4-5',
  resourceClass: 'standard',
}

const governance = {
  mode: 'strict-governed' as const,
  trusted: false,
  orkaOwnedWorkspaceDeltas: true,
  promptScopedBrokerAuthorization: true,
  noDirectSCMPublication: true,
  orkaOwnedCleanRoomPublication: true,
  exactInstanceFencing: true,
  duplicateSafeMutations: true,
  cancellationSettlement: true,
}

export const agentRuntimes: AgentRuntime[] = [
  {
    apiVersion: 'core.orka.ai/v1alpha1',
    kind: 'AgentRuntime',
    metadata: { name: 'goose-data-team', namespace: DEMO_NAMESPACE, uid: uid('agentruntime:goose'), creationTimestamp: ago(18 * 24 * 60) },
    spec: {
      contractVersion: 'orka.harness.v2',
      deployment: { mode: 'external-endpoint', endpoint: 'https://goose-acp.data.northwind.internal:8443' },
      clientAuth: {
        controllerBearerTokenSecretRef: { name: 'goose-runtime-auth', key: 'controller-token' },
        operationCapabilitySecretRef: { name: 'goose-runtime-auth', key: 'operation-capability' },
      },
      capabilities: {
        runtimeInstanceID: `rti-${digest('goose:instance', 24)}`,
        profile: gooseProfile,
        mcpPolicy: { allowedTools: ['file_read', 'web_fetch'], disallowedTools: [], allowBash: true, approvalRequiredTools: [] },
        limits,
        supportsDrain: true,
        supportsPublicationFinalization: true,
        workspaceGovernance: governance,
      },
    },
    status: {
      ready: true,
      observedGeneration: 2,
      lastValidated: ago(4),
      observedControllerAuthRefResourceVersion: '48211907',
      observedOperationCapabilityRefResourceVersion: '48211907',
      observedCapabilities: {
        protocolVersion: 'orka.harness.v2',
        transport: 'https',
        acpVersion: 'acp.v1',
        runtimeInstanceID: `rti-${digest('goose:instance', 24)}`,
        runtimeProfileDigest: gooseProfile.digest,
        profileDigestSchemaVersion: 1,
        adapterName: gooseProfile.adapterName,
        adapterDigest: gooseProfile.adapterDigest,
        providerKind: gooseProfile.providerKind,
        model: gooseProfile.model,
        supportsDrain: true,
        supportsPublicationFinalization: true,
        workspaceGovernance: governance,
        lifecycle: 'Serving',
      },
      conditions: [{ type: 'Ready', status: 'True', reason: 'CapabilitiesValidated', message: 'Runtime capabilities match the registration', lastTransitionTime: ago(4) }],
    },
  },
]
