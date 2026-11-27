import type { Task } from '@/schemas/task'
import { containsCredential, looksSecret, UNSAFE_KEY_PATTERN, WEBHOOK_URL } from './secret-like'

export const HIDDEN_VALUE = '(hidden)'

const URL_VALUE = /^[a-z][a-z\d+.-]{0,31}:\/\/\S+$/i

function withoutCredentials(value: string): string {
  let url: URL
  try {
    url = new URL(value)
  } catch {
    return HIDDEN_VALUE
  }
  let path: string
  try {
    path = decodeURIComponent(url.pathname)
  } catch {
    return `${url.origin}/${HIDDEN_VALUE}`
  }
  if (WEBHOOK_URL.test(`${url.protocol}//${url.host}${path}`) || containsCredential(path)) {
    return `${url.origin}/${HIDDEN_VALUE}`
  }
  if (!url.username && !url.password && !url.search && !url.hash) return value
  const userinfo = url.username || url.password ? `${HIDDEN_VALUE}@` : ''
  return `${url.protocol}//${userinfo}${url.host}${url.pathname}${url.search ? `?${HIDDEN_VALUE}` : ''}${url.hash ? `#${HIDDEN_VALUE}` : ''}`
}

function scrubbedText(value: string): string {
  if (URL_VALUE.test(value)) return withoutCredentials(value)
  return containsCredential(value) ? HIDDEN_VALUE : value
}

function scrubbed(value: unknown): unknown {
  if (typeof value === 'string') return scrubbedText(value)
  if (Array.isArray(value)) return value.map(scrubbed)
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, scrubbed(item)]))
  return value
}

function shown(record: Record<string, string>, dropUnsafe: boolean): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries(record)) {
    if (dropUnsafe && UNSAFE_KEY_PATTERN.test(key)) continue
    out[key] = looksSecret(key, value) ? HIDDEN_VALUE : scrubbedText(value)
  }
  return out
}

function shownWebhookURL(value: string): string {
  try {
    return `${new URL(value).origin}/${HIDDEN_VALUE}`
  } catch {
    return HIDDEN_VALUE
  }
}

export function displayedTaskResource(task: Task): Record<string, unknown> {
  const { metadata, spec, status } = task
  const { labels, annotations, ...meta } = metadata as Task['metadata'] & { managedFields?: unknown }
  delete meta.managedFields
  return {
    apiVersion: task.apiVersion || 'core.orka.ai/v1alpha1',
    kind: (task as { kind?: string }).kind || 'Task',
    metadata: {
      ...meta,
      ...(labels && { labels: shown(labels, false) }),
      ...(annotations && { annotations: shown(annotations, true) }),
    },
    spec: {
      ...(scrubbed(spec) as Task['spec']),
      ...(spec.env && { env: spec.env.map((entry) => (entry.value === undefined ? entry : { ...entry, value: HIDDEN_VALUE })) }),
      ...(spec.webhookURL && { webhookURL: shownWebhookURL(spec.webhookURL) }),
    },
    ...(status && { status: scrubbed(status) }),
  }
}
