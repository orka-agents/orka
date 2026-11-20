import type { Task } from '@/schemas/task'
import { containsCredential, looksSecret, UNSAFE_KEY_PATTERN, WEBHOOK_URL } from './secret-like'

export const HIDDEN_VALUE = '(hidden)'

// A value that is one URL and nothing else.
const URL_VALUE = /^[a-z][a-z\d+.-]{0,31}:\/\/\S+$/i

/**
 * A URL with its credentials hidden: userinfo, query and fragment, or the path
 * when it is a webhook's secret or holds a token. One that will not parse is hidden
 * whole: nothing says where its credentials end.
 */
function withoutCredentials(value: string): string {
  let url: URL
  try {
    url = new URL(value)
  } catch {
    return HIDDEN_VALUE
  }
  // A webhook's path is its secret, and a path can hold a token
  // (`/download/ghp_…`), percent-encoded or not. Checked decoded, as the server
  // reads it, and without userinfo, which would hide the host. A path that will
  // not decode is hidden.
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

/**
 * Hides credentials in a string. A value that is one URL keeps all but its
 * credentials; legacy harness-v1 repository URLs, for one, are stored without
 * the current credential rules. Other text that shows a sign of a credential
 * (a token, an Authorization header, a password assignment, a URL's userinfo
 * or signature) is hidden whole.
 */
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
 * URLs, any other text that looks like it holds a credential, secret-looking
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
