import { describe, expect, it } from 'vitest'
import { agentRuntimeListSchema, agentRuntimeSchema, runtimePoolSchema } from './runtime'

const digest = `sha256:${'a'.repeat(64)}`

const profile = {
  protocolVersion: 'orka.harness.v2' as const,
  digest,
  digestSchemaVersion: 'v1',
  acpProfile: 'schema-v1.20.0',
  adapterDigests: { adapter: digest },
  providerKind: 'openai',
  model: 'gpt-5',
  agentConfigurationDigest: digest,
  toolPolicyDigest: digest,
  approvalPolicyDigest: digest,
  mcpConfigurationDigest: digest,
  workspaceIntent: 'read' as const,
  proxyCredentialRole: 'provider',
  proxyCredentialScope: 'runtime:codex',
  resourceClass: 'standard',
}

describe('runtimePoolSchema', () => {
  it('parses v2 pool identity, capacity, and active instance status', () => {
    const value = {
      metadata: { name: 'codex-read', namespace: 'default', uid: 'pool-uid' },
      spec: {
        trustDomain: { namespace: 'default', identity: 'default/operators' },
        runtime: { image: 'docker.io/example/codex@sha256:abc', profile },
      },
      status: {
        lifecycle: 'Serving',
        admissionState: 'Accepting',
        desiredReplicas: 1,
        currentReplicas: 1,
        capacity: { maxResidentSessions: 10, maxRunningPrompts: 4, residentSessions: 2, runningPrompts: 1 },
        activeInstance: {
          podNamespace: 'orka-runtime',
          podName: 'codex-read-1',
          podAddress: '10.0.0.2',
          podUID: 'pod-uid',
          bootID: 'boot-1',
          runtimeInstanceID: 'pod-uid:boot-1',
          controllerEpoch: 7,
          protocolVersion: 'orka.harness.v2',
          profileDigest: digest,
          profileDigestSchemaVersion: 'v1',
        },
      },
    }
    expect(runtimePoolSchema.parse(value)).toEqual(value)
  })
})

describe('agentRuntimeSchema', () => {
  it('rejects a runtime without a supported contract', () => {
    const value = {
      metadata: { name: 'legacy-unclassified', namespace: 'default' },
      spec: {
        deployment: { mode: 'external-endpoint', endpoint: 'https://legacy.example.test' },
        clientAuth: { bearerTokenSecretRef: { name: 'legacy-auth', key: 'token' } },
        capabilities: { toolExecutionModes: ['observed'], supportsCancel: true },
      },
      status: { ready: false, message: 'AgentRuntime contractVersion is unclassified' },
    }

    expect(agentRuntimeSchema.safeParse(value).success).toBe(false)
  })

  it('parses current and stored pre-mcpPolicy v2 capability surfaces', () => {
    const value = {
      metadata: { name: 'external-codex', namespace: 'default' },
      spec: {
        contractVersion: 'orka.harness.v2',
        deployment: { mode: 'external-endpoint', endpoint: 'https://runtime.example.test' },
        clientAuth: {
          controllerBearerTokenSecretRef: { name: 'runtime-auth', key: 'controller-token' },
          operationCapabilitySecretRef: { name: 'runtime-auth', key: 'capability-secret' },
        },
        capabilities: {
          runtimeInstanceID: 'external-instance-1',
          profile: {
            ...profile,
            digestSchemaVersion: 1,
            adapterName: 'codex-acp',
            adapterDigest: digest,
          },
          mcpPolicy: {
            allowedTools: ['web_search'],
            disallowedTools: [],
            allowBash: false,
            approvalRequiredTools: [],
          },
          limits: {
            maxResidentSessions: 10,
            maxConcurrentPrompts: 4,
            maxRequestBytes: 1000,
            maxEventLineBytes: 1000,
            maxTerminalResultBytes: 1000,
            maxBufferedEvents: 100,
            maxUpdateEventsPerSecond: 50,
            minPromptLeaseMillis: 1000,
            maxPromptLeaseMillis: 10000,
            maxPendingPermissions: 4,
            maxWorkspaceDeltaBytes: 100000,
          },
          supportsPublicationFinalization: true,
          workspaceGovernance: {
            mode: 'strict-governed',
            trusted: false,
            orkaOwnedWorkspaceDeltas: true,
            promptScopedBrokerAuthorization: true,
            noDirectSCMPublication: true,
            orkaOwnedCleanRoomPublication: true,
            exactInstanceFencing: true,
            duplicateSafeMutations: true,
            cancellationSettlement: true,
          },
        },
      },
      status: { ready: true },
    }
    const parsed = agentRuntimeSchema.parse(value)
    expect(parsed.spec.contractVersion).toBe('orka.harness.v2')
    if (parsed.spec.contractVersion !== 'orka.harness.v2') throw new Error('expected v2 runtime')
    expect(parsed.spec.capabilities.supportsDrain).toBe(false)
    expect(parsed.spec.capabilities.profile.adapterName).toBe('codex-acp')
    expect(parsed.spec.capabilities.workspaceGovernance.mode).toBe('strict-governed')

    const { mcpPolicy, ...legacyCapabilities } = value.spec.capabilities
    expect(mcpPolicy).toBeDefined()
    const list = {
      items: [{
        ...value,
        metadata: { ...value.metadata, name: 'pre-policy-v2' },
        spec: { ...value.spec, capabilities: legacyCapabilities },
        status: { ready: false, message: 'capabilities.mcpPolicy is required' },
      }, value],
    }
    const parsedList = agentRuntimeListSchema.parse(list)
    expect(parsedList.items).toHaveLength(2)
    expect(parsedList.items[0]?.metadata.name).toBe('pre-policy-v2')
    expect(parsedList.items[0]?.spec.contractVersion).toBe('orka.harness.v2')
    if (parsedList.items[0]?.spec.contractVersion !== 'orka.harness.v2') {
      throw new Error('expected stored v2 runtime')
    }
    expect(parsedList.items[0].spec.capabilities.mcpPolicy).toBeUndefined()

    expect(agentRuntimeSchema.safeParse({
      ...value,
      spec: { ...value.spec, capabilities: { ...value.spec.capabilities, supportsContinuation: true } },
    }).success).toBe(false)
    expect(agentRuntimeSchema.safeParse({
      ...value,
      spec: { ...value.spec, contractVersion: 'orka.harness.v1' },
    }).success).toBe(false)
    expect(agentRuntimeSchema.safeParse({
      ...value,
      spec: { ...value.spec, clientAuth: { ...value.spec.clientAuth, bearerTokenSecretRef: null } },
    }).success).toBe(false)
    const currentV2Status = agentRuntimeSchema.parse({
      ...value,
      status: { ready: true, observedAuthRefResourceVersion: 'previous-auth-version' },
    })
    expect(currentV2Status.status).toEqual({ ready: true })
    expect(agentRuntimeSchema.safeParse({
      ...value,
      status: { observedCapabilities: { protocolVersion: 'orka.harness.v1' } },
    }).success).toBe(false)
    expect(agentRuntimeSchema.safeParse({
      ...value,
      status: { observedCapabilities: { supportsContinuation: true } },
    }).success).toBe(false)
  })

  it('rejects a v1 registration', () => {
    expect(agentRuntimeSchema.safeParse({
      metadata: { name: 'unsupported-runtime', namespace: 'default' },
      spec: {
        contractVersion: 'orka.harness.v1',
        deployment: { mode: 'external-endpoint', endpoint: 'https://runtime.example.test' },
        clientAuth: { bearerTokenSecretRef: { name: 'auth', key: 'token' } },
      },
    }).success).toBe(false)
  })
})
