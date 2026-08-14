import { http, HttpResponse } from 'msw'

const API = '/api/v1'

export const handlers = [
  // Tasks
  http.get(`${API}/tasks`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/tasks/:id`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.id, namespace: 'default', uid: 'uid-1' },
      spec: { type: 'container', image: 'alpine' },
      status: { phase: 'Succeeded' },
    })
  }),
  http.post(`${API}/tasks`, () => {
    return HttpResponse.json({
      metadata: { name: 'new-task', namespace: 'default' },
      spec: { type: 'container' },
    })
  }),
  http.delete(`${API}/tasks/:id`, () => {
    return new HttpResponse(null, { status: 204 })
  }),
  http.get(`${API}/tasks/:id/result`, () => {
    return HttpResponse.json({ result: 'task output' })
  }),
  http.get(`${API}/tasks/:id/children`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),

  // Execution events
  http.get(`${API}/tasks/:id/events`, ({ params }) => {
    return HttpResponse.json({
      namespace: 'default',
      streamType: 'task',
      streamID: params.id,
      afterSeq: 0,
      latestSeq: 0,
      events: [],
    })
  }),
  http.get(`${API}/tasks/:id/trace`, ({ params }) => {
    return HttpResponse.json({
      task: { namespace: 'default', name: params.id, resultAvailable: false },
      latestSeq: 0,
      generatedAt: '2026-06-13T00:00:00Z',
      timeline: [],
      modelRequests: [],
      toolCalls: [],
      childTasks: [],
      workspace: [],
      artifacts: [],
      errors: [],
      warnings: [],
    })
  }),
  http.get(`${API}/tasks/:id/approvals`, ({ params }) => {
    return HttpResponse.json({ namespace: 'default', taskName: params.id, approvals: [] })
  }),
  http.post(`${API}/tasks/:id/approvals/:approvalID/decision`, ({ params }) => {
    return HttpResponse.json({
      id: params.approvalID,
      action: 'tool',
      status: 'approved',
      createdAt: '2026-06-13T00:00:00Z',
    })
  }),
  http.post(`${API}/tasks/:id/fork`, ({ params }) => {
    return HttpResponse.json(
      {
        namespace: 'default',
        sourceTaskName: params.id,
        newTaskName: `${params.id}-fork-abcd`,
        afterSeq: 0,
        forkContext: {
          sourceNamespace: 'default',
          sourceTask: params.id,
          afterSeq: 0,
          events: [],
          truncated: false,
        },
      },
      { status: 201 },
    )
  }),
  http.get(`${API}/sessions/:id/events`, ({ params }) => {
    return HttpResponse.json({
      namespace: 'default',
      streamType: 'session',
      streamID: params.id,
      afterSeq: 0,
      latestSeq: 0,
      events: [],
    })
  }),

  // Sessions
  http.get(`${API}/sessions`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/sessions/:id`, ({ params }) => {
    return HttpResponse.json({
      name: params.id,
      namespace: 'default',
      messageCount: '5',
      inputTokens: '100',
      outputTokens: '200',
    })
  }),
  http.delete(`${API}/sessions/:id`, () => {
    return new HttpResponse(null, { status: 204 })
  }),

  // Agents
  http.get(`${API}/agents`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/agents/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: {},
      status: { activeTasks: 0 },
    })
  }),
  http.post(`${API}/agents`, () => {
    return HttpResponse.json({
      metadata: { name: 'new-agent', namespace: 'default' },
      spec: {},
    })
  }),
  http.put(`${API}/agents/:name`, () => {
    return HttpResponse.json({
      metadata: { name: 'updated', namespace: 'default' },
      spec: {},
    })
  }),
  http.delete(`${API}/agents/:name`, () => {
    return new HttpResponse(null, { status: 204 })
  }),

  // Tools
  http.get(`${API}/tools`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/tools/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: { description: 'A tool', http: { url: 'http://example.com' } },
    })
  }),

  // Secrets
  http.get(`${API}/secrets`, () => {
    return HttpResponse.json({ items: [] })
  }),

  // Auth
  http.get(`${API}/auth/validate`, () => {
    return new HttpResponse(null, { status: 200 })
  }),
  http.get(`${API}/auth/whoami`, () => {
    return HttpResponse.json({
      authenticated: true,
      authType: 'kubernetes',
      username: 'system:serviceaccount:default:orka',
      namespace: 'default',
    })
  }),

  // Task plan + children extras
  http.get(`${API}/tasks/:id/plan`, () => {
    return HttpResponse.json({
      summary: 'Working',
      progressPct: 50,
      goalComplete: false,
      planDocument: '# Plan',
      iteration: 1,
    })
  }),

  // Providers
  http.get(`${API}/providers`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/providers/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: { type: 'anthropic', secretRef: { name: 'anthropic-key' } },
      status: { ready: true },
    })
  }),
  http.post(`${API}/providers`, () => {
    return HttpResponse.json(
      { metadata: { name: 'new-provider', namespace: 'default' }, spec: { type: 'anthropic', secretRef: { name: 's' } } },
      { status: 201 },
    )
  }),
  http.put(`${API}/providers/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: { type: 'anthropic', secretRef: { name: 's' } },
    })
  }),
  http.delete(`${API}/providers/:name`, () => {
    return new HttpResponse(null, { status: 204 })
  }),

  // Skills
  http.get(`${API}/skills`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/skills/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: { description: 'A skill', content: { inline: '# Skill' } },
      status: { phase: 'Ready' },
    })
  }),
  http.get(`${API}/skills/:name/content`, () => {
    return new HttpResponse('# Skill content', {
      status: 200,
      headers: { 'Content-Type': 'text/markdown' },
    })
  }),
  http.post(`${API}/skills`, () => {
    return HttpResponse.json(
      { metadata: { name: 'new-skill', namespace: 'default' }, spec: { description: 'd', content: { inline: '#' } } },
      { status: 201 },
    )
  }),
  http.put(`${API}/skills/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: { description: 'd', content: { inline: '#' } },
    })
  }),
  http.delete(`${API}/skills/:name`, () => {
    return new HttpResponse(null, { status: 204 })
  }),

  // Memories + proposals
  http.get(`${API}/memories`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/memories/:id`, ({ params }) => {
    return HttpResponse.json({
      id: params.id,
      namespace: 'default',
      source: 'manual',
      content: 'remembered fact',
      createdAt: '2026-06-13T00:00:00Z',
      updatedAt: '2026-06-13T00:00:00Z',
    })
  }),
  http.post(`${API}/memories`, () => {
    return HttpResponse.json(
      {
        id: 'mem-1',
        namespace: 'default',
        source: 'manual',
        content: 'new memory',
        createdAt: '2026-06-13T00:00:00Z',
        updatedAt: '2026-06-13T00:00:00Z',
      },
      { status: 201 },
    )
  }),
  http.put(`${API}/memories/:id`, ({ params }) => {
    return HttpResponse.json({
      id: params.id,
      namespace: 'default',
      source: 'manual',
      content: 'updated memory',
      createdAt: '2026-06-13T00:00:00Z',
      updatedAt: '2026-06-13T00:00:00Z',
    })
  }),
  http.delete(`${API}/memories/:id`, () => {
    return new HttpResponse(null, { status: 204 })
  }),
  http.post(`${API}/memories/:id/enable`, () => {
    return new HttpResponse(null, { status: 204 })
  }),
  http.post(`${API}/memories/:id/disable`, () => {
    return new HttpResponse(null, { status: 204 })
  }),
  http.get(`${API}/memory-proposals`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/memory-proposals/:id`, ({ params }) => {
    return HttpResponse.json({
      id: params.id,
      namespace: 'default',
      type: 'memory',
      title: 'Proposal',
      status: 'pending',
      createdAt: '2026-06-13T00:00:00Z',
      updatedAt: '2026-06-13T00:00:00Z',
    })
  }),
  http.post(`${API}/memory-proposals/:id/review`, () => {
    return new HttpResponse(null, { status: 204 })
  }),
  http.post(`${API}/memory-proposals/:id/apply`, ({ params }) => {
    return HttpResponse.json({
      id: 'mem-from-proposal',
      namespace: 'default',
      source: 'proposal',
      sourceProposalId: params.id,
      content: 'applied memory',
      createdAt: '2026-06-13T00:00:00Z',
      updatedAt: '2026-06-13T00:00:00Z',
    })
  }),
  http.post(`${API}/memory-proposals/:id/archive`, () => {
    return new HttpResponse(null, { status: 204 })
  }),

  // Tools CRUD
  http.post(`${API}/tools`, () => {
    return HttpResponse.json(
      { metadata: { name: 'new-tool', namespace: 'default' }, spec: { description: 'd' } },
      { status: 201 },
    )
  }),
  http.put(`${API}/tools/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: { description: 'd' },
    })
  }),
  http.delete(`${API}/tools/:name`, () => {
    return new HttpResponse(null, { status: 204 })
  }),

  // Runtime fabric
  http.get(`${API}/substrate-actor-pools`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/substrate-actor-pools/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: {},
      status: { phase: 'Ready' },
    })
  }),
  http.post(`${API}/substrate-actor-pools`, () => {
    return HttpResponse.json(
      { metadata: { name: 'new-pool', namespace: 'default' }, spec: {} },
      { status: 201 },
    )
  }),
  http.put(`${API}/substrate-actor-pools/:name`, ({ params }) => {
    return HttpResponse.json({ metadata: { name: params.name, namespace: 'default' }, spec: {} })
  }),
  http.delete(`${API}/substrate-actor-pools/:name`, () => {
    return new HttpResponse(null, { status: 204 })
  }),
  http.post(`${API}/agent-runtimes`, () => {
    return HttpResponse.json(
      { metadata: { name: 'new-runtime', namespace: 'default' }, spec: { contractVersion: 'orka.harness.v2' } },
      { status: 201 },
    )
  }),
  http.put(`${API}/agent-runtimes/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name, namespace: 'default' },
      spec: { contractVersion: 'orka.harness.v2' },
    })
  }),
  http.delete(`${API}/agent-runtimes/:name`, () => {
    return new HttpResponse(null, { status: 204 })
  }),

  // Gateway classes
  http.get(`${API}/gatewayclasses`, () => {
    return HttpResponse.json({ items: [], metadata: {} })
  }),
  http.get(`${API}/gatewayclasses/:name`, ({ params }) => {
    return HttpResponse.json({
      metadata: { name: params.name },
      spec: { contractVersion: 'orka.gateway.v1', category: 'chat' },
      status: { accepted: true },
    })
  }),

  // Chat
  http.get(`${API}/chat/config`, () => {
    return HttpResponse.json({
      enabled: true,
      provider: 'anthropic',
      model: 'claude-sonnet-4-20250514',
      maxIterations: 10,
      maxDuration: '5m',
      maxTasksPerTurn: 3,
      maxConcurrent: 5,
      availableTools: ['create_task', 'list_tasks'],
    })
  }),
  http.delete(`${API}/chat/:sessionId`, () => {
    return new HttpResponse(null, { status: 204 })
  }),
]
