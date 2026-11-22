import { isForbiddenError } from '@/lib/api-client'

export interface ListMetadata {
  continue?: string
  remainingItemCount?: number
}

export interface ListResponse<T> {
  items: T[]
  metadata?: ListMetadata
  truncated?: boolean
}

export interface ListWalkResult<T> extends ListResponse<T> {
  metadata: ListMetadata
  truncated: boolean
}

export const maxListWalkPages = 20

export interface WalkAllPagesOptions {
  subject: string
  path?: string
  maxPages?: number
}

export async function walkAllPages<T>(
  fetchPage: (continueToken: string | undefined) => Promise<ListResponse<T>>,
  { subject, path, maxPages = Number.POSITIVE_INFINITY }: WalkAllPagesOptions,
): Promise<ListWalkResult<T>> {
  const items: T[] = []
  const seen = new Set<string>()
  let metadata: ListMetadata = {}
  let continueToken: string | undefined
  let pages = 0
  do {
    const page = await fetchPage(continueToken)
    items.push(...page.items)
    metadata = page.metadata ?? {}
    const next = metadata.continue || undefined
    if (next) {
      if (seen.has(next)) {
        throw new Error(`${subject} pagination repeated continuation cursor${path ? ` for ${path}` : ''}`)
      }
      seen.add(next)
    }
    continueToken = next
    pages += 1
  } while (continueToken && pages < maxPages)
  return { items, metadata, truncated: Boolean(continueToken) }
}

export function pageParams(params: Record<string, string>, continueToken?: string): Record<string, string> {
  return continueToken ? { ...params, continue: continueToken } : params
}

export function isPaginationProtocolError(error: unknown): boolean {
  return error instanceof Error && error.message.includes('repeated continuation cursor')
}

export const retryUnlessForbidden = (failureCount: number, error: unknown) =>
  !isForbiddenError(error) && failureCount < 3

export const pollUnlessForbidden = (interval: number | false) =>
  (query: { state: { error: unknown } }) => (isForbiddenError(query.state.error) ? false : interval)
