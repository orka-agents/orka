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
  taskExecutionStatusSchema,
  taskDeliveryStatusSchema,
  harnessRuntimeStatusSchema,
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
  it('parses canonical URL-derived repository identities and role-specific credential references', () => {
    const data = {
      intent: 'write',
      gitRepo: 'https://github.com/org/source',
      sourceRepository: { provider: 'github', id: 'github.com/org/source' },
      readCredentialRef: { name: 'repo-read', key: 'source-token' },
      publicationGitRepo: 'https://github.com/org/publish',
      publicationRepository: { provider: 'github', id: 'github.com/org/publish' },
      publicationReadCredentialRef: { name: 'repo-verify', key: 'verify-token' },
      publicationCredentialRef: { name: 'repo-write', key: 'write-token' },
      forgeCredentialRef: { name: 'repo-forge', key: 'forge-token' },
      branch: 'main',
      pushBranch: 'orka/change',
      prBaseBranch: 'main',
      createPR: true,
    }
    expect(workspaceConfigSchema.parse(data)).toEqual(data)
  })

  it('keeps credential keys optional for the API token default', () => {
    const data = { intent: 'read', readCredentialRef: { name: 'repo-read' } }
    expect(workspaceConfigSchema.parse(data)).toEqual(data)
  })

  it('requires a distinct forge credential when createPR is requested', () => {
    expect(() => workspaceConfigSchema.parse({
      intent: 'write',
      publicationCredentialRef: { name: 'repo-write' },
      createPR: true,
    })).toThrow(/createPR requires forgeCredentialRef/)
  })

  it('rejects legacy, invalid, and secret-value workspace fields', () => {
    expect(() => workspaceConfigSchema.parse({ intent: 'execute' })).toThrow()
    expect(() => workspaceConfigSchema.parse({ gitSecretRef: { name: 'legacy' } })).toThrow()
    expect(() => workspaceConfigSchema.parse({ forkRepo: 'https://example.com/fork' })).toThrow()
    expect(() => workspaceConfigSchema.parse({
      readCredentialRef: { name: 'repo-read', value: 'must-never-render' },
    })).toThrow()
  })
})

describe('agentRuntimeSpecSchema', () => {
  it('parses governed tool overrides', () => {
    const data = { allowedTools: ['read'], disallowedTools: ['write'], allowBash: false }
    expect(agentRuntimeSpecSchema.parse(data)).toEqual(data)
    expect(agentRuntimeSpecSchema.parse({ maxTurns: 50 })).toEqual({ maxTurns: 50 })
    expect(() => agentRuntimeSpecSchema.parse({ unknownField: true })).toThrow()
  })

  it('parses the preserved legacy harness v1 workspace surface', () => {
    // Stored v1 Tasks keep spec.agentRuntime.workspace as a read-only
    // coexistence compatibility surface; the CRD forbids introducing it on
    // new Tasks, but the UI must render stored objects without pruning.
    const legacy = {
      workspace: {
        gitRepo: 'https://github.com/org/repo.git',
        branch: 'main',
        gitSecretRef: { name: 'git-credentials' },
        forkRepo: 'https://github.com/bot/repo.git',
        pushBranch: 'agent/fix-1',
      },
      maxTurns: 25,
    }
    expect(agentRuntimeSpecSchema.parse(legacy)).toEqual(legacy)
  })
})

describe('structured ACP task status', () => {
  it('parses exact runtime pool execution identity', () => {
    const data = {
      state: 'Running',
      attempt: 2,
      promptID: 'prompt-2',
      runtimePoolName: 'codex-read',
      runtimeInstanceID: 'pod:boot',
      runtimeSessionGeneration: 3,
      controllerEpoch: 9,
    }
    expect(taskExecutionStatusSchema.parse(data)).toEqual(data)
  })

  it('parses publication verification and unknown outcomes', () => {
    const delivery = {
      state: 'VerifiedExact',
      outcome: 'VerifiedExact',
      publicationID: 'pub-1',
      branch: 'orka/change',
      expectedCommitSHA: 'a'.repeat(40),
      verifiedRemoteSHA: 'a'.repeat(40),
      prReceipt: { id: 'pr-1', number: 42, state: 'open' },
    }
    expect(taskDeliveryStatusSchema.parse(delivery)).toEqual(delivery)
    expect(taskExecutionStatusSchema.parse({ state: 'OutcomeUnknown', outcome: 'OutcomeUnknown' })).toEqual({
      state: 'OutcomeUnknown',
      outcome: 'OutcomeUnknown',
    })
  })

  it('preserves harness v1 reconciliation context', () => {
    const status = {
      state: 'OutcomeUnknown' as const,
      outcome: 'OutcomeUnknown' as const,
      reason: 'WrapperRestarted',
      message: 'accepted turn could not be settled after restart',
    }
    expect(harnessRuntimeStatusSchema.parse(status)).toEqual(status)
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

  it('parses the API result reference shape ({ available })', () => {
    expect(resultRefSchema.parse({ available: true })).toEqual({ available: true })
  })

  it('parses an empty result reference (all fields optional)', () => {
    expect(resultRefSchema.parse({})).toEqual({})
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
      agentRuntime: { allowBash: true },
      workspace: { intent: 'read', gitRepo: 'https://github.com/org/repo', readCredentialRef: { name: 'repo-read' } },
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
      execution: { state: 'Running', runtimePoolName: 'codex-read' },
      delivery: { state: 'Validating' },
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

  it('parses executionWorkspace status (safe fields) and ignores extras', () => {
    const status = taskStatusSchema.parse({
      phase: 'Running',
      executionWorkspace: {
        provider: 'substrate',
        phase: 'Ready',
        reused: true,
        placement: { workerPool: 'pool-a' },
        density: { actorCount: 3, actorsPerWorker: '1.5' },
        message: 'attached',
        secretToken: 'should-be-stripped',
      },
    })
    expect(status.executionWorkspace?.provider).toBe('substrate')
    expect(status.executionWorkspace?.placement?.workerPool).toBe('pool-a')
    expect((status.executionWorkspace as Record<string, unknown>).secretToken).toBeUndefined()
  })
})
