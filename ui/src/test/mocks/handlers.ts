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
