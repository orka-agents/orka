import { describe, it, expect } from 'vitest'
import {
  taskTypeSchema,
  taskPhaseSchema,
  conditionSchema,
  retryPolicySchema,
  secretRefSchema,
  sessionRefSchema,
  agentRefSchema,
  aiSpecSchema,
  workspaceConfigSchema,
  agentRuntimeSpecSchema,
  resultRefSchema,
  childTaskStatusSchema,
  taskSpecSchema,
  taskStatusSchema,
  k8sMetadataSchema,
  taskSchema,
} from './task'
import type { Task, TaskSpec, TaskStatus, TaskType, TaskPhase } from './task'

describe('taskTypeSchema', () => {
  it('parses valid values', () => {
    expect(taskTypeSchema.parse('container')).toBe('container')
    expect(taskTypeSchema.parse('ai')).toBe('ai')
    expect(taskTypeSchema.parse('agent')).toBe('agent')
  })

  it('rejects invalid values', () => {
    expect(() => taskTypeSchema.parse('invalid')).toThrow()
    expect(() => taskTypeSchema.parse(123)).toThrow()
    expect(() => taskTypeSchema.parse('')).toThrow()
  })
})

describe('taskPhaseSchema', () => {
  it('parses valid values', () => {
    expect(taskPhaseSchema.parse('Pending')).toBe('Pending')
    expect(taskPhaseSchema.parse('Running')).toBe('Running')
    expect(taskPhaseSchema.parse('Succeeded')).toBe('Succeeded')
    expect(taskPhaseSchema.parse('Failed')).toBe('Failed')
    expect(taskPhaseSchema.parse('Scheduled')).toBe('Scheduled')
    expect(taskPhaseSchema.parse('Cancelled')).toBe('Cancelled')
  })

  it('rejects invalid values', () => {
    expect(() => taskPhaseSchema.parse('pending')).toThrow()
    expect(() => taskPhaseSchema.parse('Unknown')).toThrow()
  })
})

describe('conditionSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      type: 'Ready',
      status: 'True',
      reason: 'Initialized',
      message: 'All good',
      lastTransitionTime: '2024-01-01T00:00:00Z',
    }
    expect(conditionSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { type: 'Ready', status: 'True' }
    expect(conditionSchema.parse(data)).toEqual(data)
  })

  it('rejects missing required fields', () => {
    expect(() => conditionSchema.parse({ type: 'Ready' })).toThrow()
    expect(() => conditionSchema.parse({ status: 'True' })).toThrow()
    expect(() => conditionSchema.parse({})).toThrow()
  })
})

describe('retryPolicySchema', () => {
  it('parses valid data', () => {
    const data = { maxRetries: 3, backoffMultiplier: 2, initialDelay: '5s' }
    expect(retryPolicySchema.parse(data)).toEqual(data)
  })

  it('parses empty object (all optional)', () => {
    expect(retryPolicySchema.parse({})).toEqual({})
  })

  it('rejects wrong types', () => {
    expect(() => retryPolicySchema.parse({ maxRetries: 'three' })).toThrow()
    expect(() => retryPolicySchema.parse({ backoffMultiplier: true })).toThrow()
  })
})

describe('secretRefSchema', () => {
  it('parses valid data', () => {
    const data = { name: 'my-secret', namespace: 'default' }
    expect(secretRefSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { name: 'my-secret' }
    expect(secretRefSchema.parse(data)).toEqual(data)
  })

  it('rejects missing name', () => {
    expect(() => secretRefSchema.parse({})).toThrow()
    expect(() => secretRefSchema.parse({ namespace: 'default' })).toThrow()
  })
})

describe('sessionRefSchema', () => {
  it('parses valid data with all fields', () => {
    const data = { name: 'sess-1', create: true, append: false, maxMessages: 100 }
    expect(sessionRefSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    expect(sessionRefSchema.parse({ name: 'sess-1' })).toEqual({ name: 'sess-1' })
  })

  it('rejects missing name', () => {
    expect(() => sessionRefSchema.parse({})).toThrow()
  })

  it('rejects wrong types for optional fields', () => {
    expect(() => sessionRefSchema.parse({ name: 'sess-1', create: 'yes' })).toThrow()
    expect(() => sessionRefSchema.parse({ name: 'sess-1', maxMessages: 'many' })).toThrow()
  })
})

describe('agentRefSchema', () => {
  it('parses valid data', () => {
    const data = { name: 'my-agent', namespace: 'default' }
    expect(agentRefSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    expect(agentRefSchema.parse({ name: 'my-agent' })).toEqual({ name: 'my-agent' })
  })

  it('rejects missing name', () => {
    expect(() => agentRefSchema.parse({})).toThrow()
  })
})

describe('aiSpecSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      providerRef: { name: 'openai', namespace: 'default' },
      provider: 'openai',
      model: 'gpt-4',
      prompt: 'Hello',
      systemPrompt: 'You are helpful',
      temperature: 0.7,
      maxTokens: 1000,
      skills: [{ configMapRef: { name: 'skill-1', key: 'skill.md' } }],
      tools: ['tool-1', 'tool-2'],
    }
    expect(aiSpecSchema.parse(data)).toEqual(data)
  })

  it('parses empty object (all optional)', () => {
    expect(aiSpecSchema.parse({})).toEqual({})
  })

  it('rejects wrong types', () => {
    expect(() => aiSpecSchema.parse({ temperature: 'hot' })).toThrow()
    expect(() => aiSpecSchema.parse({ tools: 'tool-1' })).toThrow()
  })

  it('parses providerRef with only name', () => {
    expect(aiSpecSchema.parse({ providerRef: { name: 'openai' } })).toEqual({
      providerRef: { name: 'openai' },
    })
  })
})

describe('workspaceConfigSchema', () => {
  it('parses valid data', () => {
    const data = {
      gitRepo: 'https://github.com/org/repo',
      branch: 'main',
      ref: 'abc123',
      gitSecretRef: { name: 'git-creds' },
      subPath: 'src/',
    }
    expect(workspaceConfigSchema.parse(data)).toEqual(data)
  })

  it('parses empty object', () => {
    expect(workspaceConfigSchema.parse({})).toEqual({})
  })

  it('rejects wrong types', () => {
    expect(() => workspaceConfigSchema.parse({ gitSecretRef: 'invalid' })).toThrow()
  })
})

describe('agentRuntimeSpecSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      workspace: { gitRepo: 'https://github.com/org/repo', branch: 'main' },
      maxTurns: 50,
      allowedTools: ['bash', 'read'],
      disallowedTools: ['write'],
      allowBash: true,
    }
    expect(agentRuntimeSpecSchema.parse(data)).toEqual(data)
  })

  it('parses empty object', () => {
    expect(agentRuntimeSpecSchema.parse({})).toEqual({})
  })

  it('rejects wrong types', () => {
    expect(() => agentRuntimeSpecSchema.parse({ maxTurns: 'fifty' })).toThrow()
    expect(() => agentRuntimeSpecSchema.parse({ allowBash: 'yes' })).toThrow()
  })
})

describe('resultRefSchema', () => {
  it('parses valid data', () => {
    const data = { configMapName: 'result-cm', key: 'output' }
    expect(resultRefSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    expect(resultRefSchema.parse({ configMapName: 'result-cm' })).toEqual({ configMapName: 'result-cm' })
  })

  it('rejects missing configMapName', () => {
    expect(() => resultRefSchema.parse({})).toThrow()
  })
})

describe('childTaskStatusSchema', () => {
  it('parses valid data', () => {
    const data = { name: 'child-1', agent: 'agent-1', phase: 'Running', result: 'ok' }
    expect(childTaskStatusSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    const data = { name: 'child-1', agent: 'agent-1', phase: 'Succeeded' }
    expect(childTaskStatusSchema.parse(data)).toEqual(data)
  })

  it('rejects invalid phase', () => {
    expect(() => childTaskStatusSchema.parse({ name: 'c', agent: 'a', phase: 'invalid' })).toThrow()
  })

  it('rejects missing required fields', () => {
    expect(() => childTaskStatusSchema.parse({ name: 'c', phase: 'Running' })).toThrow()
  })
})

describe('taskSpecSchema', () => {
  it('parses valid container task', () => {
    const data = {
      type: 'container',
      image: 'alpine:latest',
      command: ['echo'],
      args: ['hello'],
      timeout: '30s',
      priority: 100,
    }
    expect(taskSpecSchema.parse(data)).toEqual(data)
  })

  it('parses valid ai task', () => {
    const data = {
      type: 'ai',
      ai: { provider: 'openai', model: 'gpt-4', prompt: 'test' },
      sessionRef: { name: 'sess-1' },
    }
    expect(taskSpecSchema.parse(data)).toEqual(data)
  })

  it('parses valid agent task', () => {
    const data = {
      type: 'agent',
      agentRef: { name: 'my-agent' },
      prompt: 'do something',
      agentRuntime: { maxTurns: 10, allowBash: true },
    }
    expect(taskSpecSchema.parse(data)).toEqual(data)
  })

  it('parses minimal spec (only type required)', () => {
    expect(taskSpecSchema.parse({ type: 'container' })).toEqual({ type: 'container' })
  })

  it('rejects missing type', () => {
    expect(() => taskSpecSchema.parse({})).toThrow()
  })

  it('rejects invalid type', () => {
    expect(() => taskSpecSchema.parse({ type: 'invalid' })).toThrow()
  })

  it('handles env array', () => {
    const data = {
      type: 'container',
      env: [{ name: 'FOO', value: 'bar' }, { name: 'BAZ' }],
    }
    const result = taskSpecSchema.parse(data)
    expect(result.env).toHaveLength(2)
  })
})

describe('taskStatusSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      phase: 'Running',
      startTime: '2024-01-01T00:00:00Z',
      completionTime: '2024-01-01T01:00:00Z',
      attempts: 2,
      jobName: 'task-xyz-job',
      resultRef: { configMapName: 'result-cm' },
      webhookDelivered: true,
      message: 'Task completed',
      childTasks: [{ name: 'c1', agent: 'a1', phase: 'Succeeded' }],
      conditions: [{ type: 'Ready', status: 'True' }],
    }
    expect(taskStatusSchema.parse(data)).toEqual(data)
  })

  it('parses empty object (all optional)', () => {
    expect(taskStatusSchema.parse({})).toEqual({})
  })

  it('rejects invalid phase', () => {
    expect(() => taskStatusSchema.parse({ phase: 'invalid' })).toThrow()
  })
})

describe('k8sMetadataSchema', () => {
  it('parses valid data with all fields', () => {
    const data = {
      name: 'my-task',
      namespace: 'default',
      uid: '123e4567-e89b-12d3-a456-426614174000',
      creationTimestamp: '2024-01-01T00:00:00Z',
      labels: { app: 'test' },
      annotations: { note: 'value' },
    }
    expect(k8sMetadataSchema.parse(data)).toEqual(data)
  })

  it('parses with only required fields', () => {
    expect(k8sMetadataSchema.parse({ name: 'my-task' })).toEqual({ name: 'my-task' })
  })

  it('rejects missing name', () => {
    expect(() => k8sMetadataSchema.parse({})).toThrow()
  })

  it('rejects invalid labels type', () => {
    expect(() => k8sMetadataSchema.parse({ name: 'x', labels: 'invalid' })).toThrow()
  })
})

describe('taskSchema', () => {
  it('parses valid full task', () => {
    const data = {
      apiVersion: 'core.orka.ai/v1alpha1',
      kind: 'Task',
      metadata: { name: 'my-task', namespace: 'default' },
      spec: { type: 'container', image: 'alpine' },
      status: { phase: 'Succeeded' },
    }
    expect(taskSchema.parse(data)).toEqual(data)
  })

  it('parses minimal task', () => {
    const data = {
      metadata: { name: 'my-task' },
      spec: { type: 'ai' },
    }
    expect(taskSchema.parse(data)).toEqual(data)
  })

  it('rejects missing metadata', () => {
    expect(() => taskSchema.parse({ spec: { type: 'container' } })).toThrow()
  })

  it('rejects missing spec', () => {
    expect(() => taskSchema.parse({ metadata: { name: 'x' } })).toThrow()
  })
})

describe('exported types', () => {
  it('Task type matches schema', () => {
    const task: Task = {
      metadata: { name: 'test' },
      spec: { type: 'container' },
    }
    expect(taskSchema.parse(task)).toBeDefined()
  })

  it('TaskSpec type matches schema', () => {
    const spec: TaskSpec = { type: 'ai' }
    expect(taskSpecSchema.parse(spec)).toBeDefined()
  })

  it('TaskStatus type matches schema', () => {
    const status: TaskStatus = { phase: 'Running' }
    expect(taskStatusSchema.parse(status)).toBeDefined()
  })

  it('TaskType type matches schema', () => {
    const t: TaskType = 'container'
    expect(taskTypeSchema.parse(t)).toBe('container')
  })

  it('TaskPhase type matches schema', () => {
    const p: TaskPhase = 'Failed'
    expect(taskPhaseSchema.parse(p)).toBe('Failed')
  })
})
