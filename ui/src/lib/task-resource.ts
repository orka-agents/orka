import type { Task } from '@/schemas/task'
import { looksSecret, UNSAFE_KEY_PATTERN, WEBHOOK_URL_IN_TEXT } from './secret-like'

export const HIDDEN_VALUE = '(hidden)'

// A value that is one URL and nothing else.
const URL_VALUE = /^[a-z][a-z\d+.-]*:\/\/\S+$/i
// `user:token@` after a scheme, anywhere in a string (a git error in a message).
// Greedy up to the last `@` before the path, as URL parsers read a password
// that itself contains `@`.
const USERINFO_IN_TEXT = /\b([a-z][a-z\d+.-]*:\/\/)[^\s/?#]*@/gi

/**
 * Hides the userinfo, query and fragment of a URL value, where credentials
 * ride (`user:token@`, signed query strings); userinfo in a URL inside other
 * text; and the secret path of a known webhook URL anywhere. Legacy harness-v1 repository URLs, for one, are stored without
 * the current credential rules.
 */
function withoutURLCredentials(value: string): string {
  value = value.replace(WEBHOOK_URL_IN_TEXT, `$1${HIDDEN_VALUE}`)
  if (!URL_VALUE.test(value)) return value.replace(USERINFO_IN_TEXT, `$1${HIDDEN_VALUE}@`)
  let url: URL
  try {
    url = new URL(value)
  } catch {
    // Shaped like a URL but unparsable: nothing says where its credentials end.
    return HIDDEN_VALUE
  }
  if (!url.username && !url.password && !url.search && !url.hash) return value
  const userinfo = url.username || url.password ? `${HIDDEN_VALUE}@` : ''
  return `${url.protocol}//${userinfo}${url.host}${url.pathname}${url.search ? `?${HIDDEN_VALUE}` : ''}${url.hash ? `#${HIDDEN_VALUE}` : ''}`
}

function scrubbed(value: unknown): unknown {
  if (typeof value === 'string') return withoutURLCredentials(value)
  if (Array.isArray(value)) return value.map(scrubbed)
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, scrubbed(item)]))
  return value
}

function shown(record: Record<string, string>, dropUnsafe: boolean): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries(record)) {
    if (dropUnsafe && UNSAFE_KEY_PATTERN.test(key)) continue
    out[key] = looksSecret(key, value) ? HIDDEN_VALUE : withoutURLCredentials(value)
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
 * displays: environment values, the webhook URL past its host, credentials in
 * any other URL, secret-looking labels and annotations, and kubectl's
 * annotations (last-applied embeds the whole spec, env included).
 * The API's `plan` is not part of the resource and is left out.
 */
export function displayedTaskResource(task: Task): Record<string, unknown> {
  const { metadata, spec, status } = task
  const { labels, annotations, ...meta } = metadata as Task['metadata'] & { managedFields?: unknown }
  // Server-side apply bookkeeping; the API drops it, and so does this view.
  delete meta.managedFields
  return {
    // The API's cached reads often leave out the type; Tasks are core.orka.ai.
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
