import { describe, expect, it } from 'vitest'
import { parse, stringify } from 'yaml'
import type { Task } from '@/schemas/task'
import { displayedTaskResource, HIDDEN_VALUE } from './task-resource'

const task = {
  metadata: {
    name: 'fix-bug',
    namespace: 'team-a',
    managedFields: [{ manager: 'kubectl' }],
    labels: { 'orka.ai/agent': 'fixer', 'auth-token': 'abc' },
    annotations: {
      'orka.ai/created-by': 'maya',
      'kubectl.kubernetes.io/last-applied-configuration': '{"spec":{"env":[{"name":"API_KEY","value":"sk-live"}]}}',
      'orka.ai/note': 'Bearer abc.def',
      'example.com/config': '{"password":"hunter2"}',
      'example.com/blob': `{"settings":"${'x'.repeat(100)}"}`,
    },
  },
  spec: { type: 'container', image: 'busybox', env: [{ name: 'API_KEY', value: 'sk-live-123456' }, { name: 'FROM_SECRET' }] },
  status: { phase: 'Running' },
  plan: { summary: 'not part of the resource' },
} as unknown as Task

describe('displayedTaskResource', () => {
  const shown = displayedTaskResource(task)

  it('names the resource and keeps its spec and status, but not the API-only plan', () => {
    expect(shown).toMatchObject({ apiVersion: 'orka.ai/v1alpha1', kind: 'Task', spec: { type: 'container', image: 'busybox' }, status: { phase: 'Running' } })
    expect(shown).not.toHaveProperty('plan')
    expect(shown.metadata).not.toHaveProperty('managedFields')
  })

  it('hides environment values and secret-looking labels and annotations', () => {
    expect((shown.spec as Task['spec']).env).toEqual([{ name: 'API_KEY', value: HIDDEN_VALUE }, { name: 'FROM_SECRET' }])
    expect(shown.metadata).toMatchObject({
      labels: { 'orka.ai/agent': 'fixer', 'auth-token': HIDDEN_VALUE },
      annotations: { 'orka.ai/created-by': 'maya', 'orka.ai/note': HIDDEN_VALUE, 'example.com/config': HIDDEN_VALUE, 'example.com/blob': HIDDEN_VALUE },
    })
    expect(JSON.stringify(shown)).not.toMatch(/sk-live|last-applied|Bearer|hunter2|xxxx/)
  })

  it('writes YAML that reads back as the same object', () => {
    expect(parse(stringify(shown))).toEqual(shown)
  })
})
