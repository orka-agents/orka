import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'
import userEvent from '@testing-library/user-event'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

import { useUIStore } from '@/stores/ui'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { AgentRuntimeActions, RegisterRuntimeButton } from './agent-runtime-registration'
import type { AgentRuntime } from '@/schemas/runtime'

describe('RegisterRuntimeButton', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('registers a runtime from the seeded manifest', async () => {
    let posted: any
    server.use(
      http.post('/api/v1/agent-runtimes', async ({ request }) => {
        posted = await request.json()
        return HttpResponse.json({ metadata: { name: 'my-runtime', namespace: 'default' }, spec: posted.spec }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    render(<RegisterRuntimeButton />)
    await user.click(screen.getByRole('button', { name: /register runtime/i }))
    expect(await screen.findByLabelText('Manifest YAML')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /^register$/i }))
    await waitFor(() => expect(posted).toBeTruthy())
    expect(posted.metadata.namespace).toBe('default')
    expect(posted.metadata.name).toBe('my-runtime')
    expect(posted.spec.contractVersion).toBe('orka.harness.v2')
  })
})

describe('AgentRuntimeActions', () => {
  const runtime: AgentRuntime = {
    metadata: { name: 'byo', namespace: 'default' },
    spec: {
      contractVersion: 'orka.harness.v2',
      deployment: { mode: 'external-endpoint', endpoint: 'https://runtime.internal' },
      capabilities: {
        runtimeInstanceID: 'i-1',
        profile: {
          digest: 'sha256:abc',
          acpProfile: 'acp.v1',
          providerKind: 'claude',
          model: 'claude-sonnet-4',
          workspaceIntent: 'read',
        },
        limits: { maxResidentSessions: 10, maxConcurrentPrompts: 4 },
        workspaceGovernance: { mode: 'strict-governed' },
      },
    },
  } as AgentRuntime

  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
  })

  it('updates the spec through the manifest editor', async () => {
    let putBody: any
    server.use(
      http.put('/api/v1/agent-runtimes/:name', async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json({ metadata: { name: 'byo', namespace: 'default' }, spec: putBody.spec })
      }),
    )
    const user = userEvent.setup()
    render(<AgentRuntimeActions runtime={runtime} />)
    await user.click(screen.getByRole('button', { name: /edit spec/i }))
    expect(await screen.findByLabelText('Manifest YAML')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /save changes/i }))
    await waitFor(() => expect(putBody).toBeTruthy())
    expect(putBody.spec.deployment.endpoint).toBe('https://runtime.internal')
  })

  it('removes a runtime after two-step confirm', async () => {
    let deleted = false
    server.use(
      http.delete('/api/v1/agent-runtimes/:name', () => {
        deleted = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    render(<AgentRuntimeActions runtime={runtime} />)
    await user.click(screen.getByRole('button', { name: /^remove$/i }))
    expect(deleted).toBe(false)
    await user.click(screen.getByRole('button', { name: /confirm remove/i }))
    await waitFor(() => expect(deleted).toBe(true))
  })
})
