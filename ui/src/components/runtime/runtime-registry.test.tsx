import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'
import { RuntimeRegistry } from './runtime-registry'

vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

const digest = `sha256:${'a'.repeat(64)}`

function pool() {
  return {
    metadata: { name: 'codex-read', namespace: 'default', uid: 'pool-uid' },
    spec: {
      trustDomain: { namespace: 'default', identity: 'default/operators' },
      desiredReplicas: 1,
      runtime: {
        image: 'docker.io/sozercan/orka-acp-codex@sha256:abc',
        profile: {
          protocolVersion: 'orka.harness.v2', digest, digestSchemaVersion: 'v1', acpProfile: 'schema-v1.20.0',
          adapterDigests: { 'codex-acp': digest }, providerKind: 'openai', model: 'gpt-5',
          agentConfigurationDigest: digest, toolPolicyDigest: digest, approvalPolicyDigest: digest,
          mcpConfigurationDigest: digest, workspaceIntent: 'read', proxyCredentialRole: 'provider',
          proxyCredentialScope: 'codex', resourceClass: 'standard',
        },
      },
    },
    status: {
      lifecycle: 'Serving', admissionState: 'Accepting', desiredReplicas: 1, currentReplicas: 1,
      capacity: { maxResidentSessions: 10, maxRunningPrompts: 4, residentSessions: 3, runningPrompts: 2, queuedTasks: 1 },
    },
  }
}

function externalRuntime() {
  return {
    metadata: { name: 'external-codex', namespace: 'default', uid: 'runtime-uid' },
    spec: {
      contractVersion: 'orka.harness.v2',
      deployment: { mode: 'external-endpoint', endpoint: 'https://runtime.example.test' },
      clientAuth: {
        controllerBearerTokenSecretRef: { name: 'auth', key: 'controller-token' },
        operationCapabilitySecretRef: { name: 'auth', key: 'capability-secret' },
      },
      capabilities: {
        runtimeInstanceID: 'external-1',
        profile: {
          digest, digestSchemaVersion: 1, acpProfile: 'schema-v1.20.0', adapterName: 'codex-acp', adapterDigest: digest,
          providerKind: 'openai', model: 'gpt-5', agentConfigurationDigest: digest, toolPolicyDigest: digest,
          approvalPolicyDigest: digest, mcpConfigurationDigest: digest, workspaceIntent: 'read',
          proxyCredentialRole: 'provider', proxyCredentialScope: 'codex', resourceClass: 'standard',
        },
        limits: {
          maxResidentSessions: 10, maxConcurrentPrompts: 4, maxRequestBytes: 1000, maxEventLineBytes: 1000,
          maxTerminalResultBytes: 1000, maxBufferedEvents: 100, maxUpdateEventsPerSecond: 50,
          minPromptLeaseMillis: 1000, maxPromptLeaseMillis: 10000, maxPendingPermissions: 4, maxWorkspaceDeltaBytes: 100000,
        },
        supportsDrain: true,
        supportsPublicationFinalization: true,
        workspaceGovernance: {
          mode: 'strict-governed', trusted: false, orkaOwnedWorkspaceDeltas: true,
          promptScopedBrokerAuthorization: true, noDirectSCMPublication: true,
          orkaOwnedCleanRoomPublication: true, exactInstanceFencing: true,
          duplicateSafeMutations: true, cancellationSettlement: true,
        },
      },
    },
    status: { ready: true, lastValidated: '2026-07-24T00:00:00Z' },
  }
}

describe('RuntimeRegistry', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
    server.use(
      http.get('/api/v1/runtime-pools', () => HttpResponse.json({ items: [pool()], metadata: {} })),
      http.get('/api/v1/agent-runtimes', () => HttpResponse.json({ items: [externalRuntime()], metadata: {} })),
    )
  })

  it('renders pool admission, capacity, and profile identity', async () => {
    render(<RuntimeRegistry />)
    await waitFor(() => expect(screen.getByText('codex-read')).toBeInTheDocument())
    expect(screen.getByText('Serving')).toBeInTheDocument()
    expect(screen.getByText('Accepting')).toBeInTheDocument()
    expect(screen.getByText('3 / 10')).toBeInTheDocument()
    expect(screen.getByText('2 / 4')).toBeInTheDocument()
    expect(screen.getByText('schema-v1.20.0')).toBeInTheDocument()
  })

  it('renders the external v2 governance surface without v1 fields', async () => {
    const user = userEvent.setup()
    render(<RuntimeRegistry />)
    await user.click(screen.getByRole('tab', { name: 'External runtimes' }))
    await waitFor(() => expect(screen.getByText('external-codex')).toBeInTheDocument())
    expect(screen.getByText('orka.harness.v2')).toBeInTheDocument()
    expect(screen.getByText('strict-governed')).toBeInTheDocument()
    expect(screen.getByText('Exact-instance fencing')).toBeInTheDocument()
    expect(screen.queryByText(/continuation/i)).not.toBeInTheDocument()
  })
})
