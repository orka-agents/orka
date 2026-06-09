import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@/test/test-utils'

vi.mock('zustand/middleware', () => ({
  persist: (fn: unknown) => fn,
}))

vi.mock('@tanstack/react-router', async () => {
  const actual = await vi.importActual('@tanstack/react-router')
  return {
    ...actual,
    Link: ({ children, to, ...props }: any) => <a href={to} {...props}>{children}</a>,
    useNavigate: () => vi.fn(),
    useLocation: () => ({ pathname: '/tools' }),
  }
})

import { useUIStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { http, HttpResponse } from 'msw'
import { server } from '@/test/mocks/server'
import { ToolDetail } from './tool-detail'

describe('ToolDetail', () => {
  beforeEach(() => {
    useUIStore.setState({ namespace: 'default', sidebarCollapsed: false, theme: 'light' })
    useAuthStore.setState({ token: 'test-token' })
  })

  it('loading state shows skeletons', () => {
    const { container } = render(<ToolDetail toolName="some-tool" />)
    const skeletons = container.querySelectorAll('[class*="animate-pulse"], [data-slot="skeleton"]')
    expect(skeletons.length).toBeGreaterThan(0)
  })

  it('not found shows "Tool not found"', async () => {
    server.use(
      http.get('/api/v1/tools/:name', () => new HttpResponse(null, { status: 404 }))
    )

    render(<ToolDetail toolName="nonexistent" />)
    await waitFor(() => {
      expect(screen.getByText('Tool not found')).toBeInTheDocument()
    })
  })

  it('custom tool shows name, description, HTTP config, parameters, and status', async () => {
    server.use(
      http.get('/api/v1/tools/:name', () =>
        HttpResponse.json({
          metadata: { name: 'my-tool', namespace: 'default', uid: 'uid-1' },
          spec: {
            description: 'A custom tool',
            http: { url: 'http://example.com/api', method: 'POST', timeout: '30s', headers: { 'X-Custom': 'value' } },
            parameters: { type: 'object', properties: { query: { type: 'string' } } },
          },
          status: { available: true, lastCheck: '2025-01-01T00:00:00Z' },
        })
      )
    )

    render(<ToolDetail toolName="my-tool" />)
    await waitFor(() => {
      expect(screen.getByText('my-tool')).toBeInTheDocument()
    })

    // Description
    expect(screen.getByText('A custom tool')).toBeInTheDocument()

    // Custom badge
    expect(screen.getByText('Custom')).toBeInTheDocument()

    // HTTP config
    expect(screen.getByText('HTTP Configuration')).toBeInTheDocument()
    expect(screen.getByText('http://example.com/api')).toBeInTheDocument()
    expect(screen.getByText('POST')).toBeInTheDocument()
    expect(screen.getByText('30s')).toBeInTheDocument()

    // Headers
    expect(screen.getByText(/X-Custom/)).toBeInTheDocument()

    // Parameters
    expect(screen.getByText('Parameters (JSON Schema)')).toBeInTheDocument()
    expect(screen.getByText(/query/)).toBeInTheDocument()

    // Status
    expect(screen.getByText('Status')).toBeInTheDocument()
    expect(screen.getByText('Yes')).toBeInTheDocument()
  })

  it('custom tool with unavailable status shows "No" badge and error', async () => {
    server.use(
      http.get('/api/v1/tools/:name', () =>
        HttpResponse.json({
          metadata: { name: 'broken-tool', namespace: 'default', uid: 'uid-2' },
          spec: {
            description: 'Broken tool',
            http: { url: 'http://broken.example.com' },
          },
          status: { available: false, lastCheck: '2025-01-01T00:00:00Z', error: 'Connection refused' },
        })
      )
    )

    render(<ToolDetail toolName="broken-tool" />)
    await waitFor(() => {
      expect(screen.getByText('broken-tool')).toBeInTheDocument()
    })
    expect(screen.getByText('No')).toBeInTheDocument()
    expect(screen.getByText('Connection refused')).toBeInTheDocument()
  })

  it('MCP-only custom tool shows actor configuration without HTTP config', async () => {
    server.use(
      http.get('/api/v1/tools/:name', () =>
        HttpResponse.json({
          metadata: { name: 'mcp-tool', namespace: 'default', uid: 'uid-3' },
          spec: {
            description: 'Durable MCP tool',
            mcp: {
              path: '/mcp',
              substrateActor: {
                templateRef: { name: 'mcp-template', namespace: 'ate-demo' },
                poolRef: { name: 'mcp-pool', namespace: 'default' },
              },
            },
          },
          status: {
            available: true,
            endpoint: 'http://router/mcp',
            actor: {
              actorID: 'orka-p-pool-00001',
              routeHost: 'orka-p-pool-00001.actors.example.com',
              poolRef: { name: 'mcp-pool', namespace: 'default' },
            },
          },
        })
      )
    )

    render(<ToolDetail toolName="mcp-tool" />)
    await waitFor(() => {
      expect(screen.getByText('mcp-tool')).toBeInTheDocument()
    })

    expect(screen.getByText('MCP Actor Configuration')).toBeInTheDocument()
    expect(screen.getByText('/mcp')).toBeInTheDocument()
    expect(screen.getByText('http://router/mcp')).toBeInTheDocument()
    expect(screen.getByText('ate-demo/mcp-template')).toBeInTheDocument()
    expect(screen.getAllByText('default/mcp-pool').length).toBeGreaterThan(0)
    expect(screen.getByText('orka-p-pool-00001')).toBeInTheDocument()
    expect(screen.getByText('orka-p-pool-00001.actors.example.com')).toBeInTheDocument()
    expect(screen.queryByText('HTTP Configuration')).not.toBeInTheDocument()
  })

  it('MCP custom tool with transport auth omits empty HTTP URL row', async () => {
    server.use(
      http.get('/api/v1/tools/:name', () =>
        HttpResponse.json({
          metadata: { name: 'mcp-auth-tool', namespace: 'default', uid: 'uid-4' },
          spec: {
            description: 'Authenticated MCP tool',
            http: { authSecretRef: { name: 'mcp-auth', key: 'token' }, authInject: 'header' },
            mcp: {
              path: '/mcp',
              substrateActor: {
                templateRef: { name: 'mcp-template', namespace: 'ate-demo' },
              },
            },
          },
          status: {
            available: true,
            endpoint: 'http://router/mcp',
          },
        })
      )
    )

    render(<ToolDetail toolName="mcp-auth-tool" />)
    await waitFor(() => {
      expect(screen.getByText('mcp-auth-tool')).toBeInTheDocument()
    })

    expect(screen.getByText('HTTP Configuration')).toBeInTheDocument()
    expect(screen.queryByText('URL:')).not.toBeInTheDocument()
    expect(screen.getByText('Auth Inject:')).toBeInTheDocument()
    expect(screen.getByText('header')).toBeInTheDocument()
    expect(screen.getByText('MCP Actor Configuration')).toBeInTheDocument()
  })

  it('built-in tool shows name, "Built-in" badge, and description', async () => {
    server.use(
      http.get('/api/v1/tools/:name', () =>
        HttpResponse.json({
          name: 'create_task',
          builtin: true,
          description: 'Create a new task',
        })
      )
    )

    render(<ToolDetail toolName="create_task" />)
    await waitFor(() => {
      expect(screen.getByText('create_task')).toBeInTheDocument()
    })

    expect(screen.getByText('Built-in')).toBeInTheDocument()
    expect(screen.getByText('Create a new task')).toBeInTheDocument()

    // Should NOT show HTTP Configuration for built-in tools
    expect(screen.queryByText('HTTP Configuration')).not.toBeInTheDocument()
  })
})
