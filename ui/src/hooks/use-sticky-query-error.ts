import { useState } from 'react'

interface QueryErrorState<TError> {
  data: unknown
  error: TError | null
  errorUpdatedAt: number
}

export function useStickyQueryError<TError>(query: QueryErrorState<TError>): TError | null {
  const [last, setLast] = useState<{ error: TError; at: number } | null>(null)
  if (query.error && last?.error !== query.error) {
    setLast({ error: query.error, at: query.errorUpdatedAt })
  }
  return query.error ?? (query.data === undefined && last?.at === query.errorUpdatedAt ? last.error : null)
}
