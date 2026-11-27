interface StatusCondition {
  type: string
  status: string
  reason?: string
  message?: string
}

export interface StatusProblem {
  reason: string
  message?: string
}

export function statusProblem(status?: { phase?: string; conditions?: StatusCondition[] }): StatusProblem | undefined {
  if (status?.phase !== 'Error') return undefined
  const ready = status.conditions?.find((condition) => condition.type === 'Ready')
  if (ready?.status !== 'False' || !ready.reason) return undefined
  return { reason: ready.reason, message: ready.message || undefined }
}
