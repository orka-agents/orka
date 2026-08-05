import { describe, expect, it } from 'vitest'
import { agentRuntimeSchema, runtimePoolSchema } from './runtime'

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
  it('parses only the v2 capability surface', () => {
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
          supportsDrain: true,
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
    const parsed = agentRuntimeSchema.parse({
      ...value,
      spec: { ...value.spec, capabilities: { ...value.spec.capabilities, supportsContinuation: true } },
    })
    expect(parsed.spec.contractVersion).toBe('orka.harness.v2')
    expect(parsed.spec.capabilities.profile.adapterName).toBe('codex-acp')
    expect(parsed.spec.capabilities.workspaceGovernance.mode).toBe('strict-governed')
    expect((parsed.spec.capabilities as Record<string, unknown>).supportsContinuation).toBeUndefined()
  })

  it('rejects the v1 contract', () => {
    expect(() => agentRuntimeSchema.parse({ metadata: { name: 'legacy' }, spec: { contractVersion: 'orka.harness.v1' } })).toThrow()
  })
})
