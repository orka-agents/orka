import type { Task } from '@/schemas/task'
import { looksSecret, UNSAFE_KEY_PATTERN } from './secret-like'

export const HIDDEN_VALUE = '(hidden)'

function shown(record: Record<string, string>, dropUnsafe: boolean): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries(record)) {
    if (dropUnsafe && UNSAFE_KEY_PATTERN.test(key)) continue
    out[key] = looksSecret(key, value) ? HIDDEN_VALUE : value
  }
  return out
}

/**
 * A webhook URL can carry its secret in the path (Slack), the query (signed
 * URLs) or userinfo, so only the scheme and host are shown.
 */
function shownWebhookURL(value: string): string {
  try {
    return `${new URL(value).origin}/${HIDDEN_VALUE}`
  } catch {
    return HIDDEN_VALUE
  }
}

/**
 * The Task resource as kubectl shows it, minus what the dashboard never
 * displays: environment values, the webhook URL past its host, secret-looking
 * labels and annotations, and kubectl's annotations (last-applied embeds the
 * whole spec, env included).
 * The API's `plan` is not part of the resource and is left out.
 */
export function displayedTaskResource(task: Task): Record<string, unknown> {
  const { metadata, spec, status } = task
  const { labels, annotations, ...meta } = metadata as Task['metadata'] & { managedFields?: unknown }
  // Server-side apply bookkeeping; the API drops it, and so does this view.
  delete meta.managedFields
  return {
    apiVersion: task.apiVersion || 'orka.ai/v1alpha1',
    kind: (task as { kind?: string }).kind || 'Task',
    metadata: {
      ...meta,
      ...(labels && { labels: shown(labels, false) }),
      ...(annotations && { annotations: shown(annotations, true) }),
    },
    spec: {
      ...spec,
      ...(spec.env && { env: spec.env.map((entry) => (entry.value === undefined ? entry : { ...entry, value: HIDDEN_VALUE })) }),
      ...(spec.webhookURL && { webhookURL: shownWebhookURL(spec.webhookURL) }),
    },
    ...(status && { status }),
  }
}
