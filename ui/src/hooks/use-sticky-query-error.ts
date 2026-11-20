import { useState } from 'react'

interface QueryErrorState<TError> {
  data: unknown
  error: TError | null
  errorUpdatedAt: number
}

/**
 * React Query puts a query that never loaded back to pending, clearing its
 * error, each time it polls again. Keep the query's last failure (matched by
 * its error timestamp) until a load succeeds, so a failed first load keeps
 * its error state instead of flipping back to a loading skeleton.
 */
export function useStickyQueryError<TError>(query: QueryErrorState<TError>): TError | null {
  const [last, setLast] = useState<{ error: TError; at: number } | null>(null)
  if (query.error && last?.error !== query.error) {
    setLast({ error: query.error, at: query.errorUpdatedAt })
  }
  return query.error ?? (query.data === undefined && last?.at === query.errorUpdatedAt ? last.error : null)
}
