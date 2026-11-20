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

/**
 * Why a resource in the Error phase is not ready. The RepositoryScan and
 * RepositoryMonitor controllers record the cause (InvalidRepository,
 * InvalidSchedule, ScanFailed, ...) only on the Ready=False condition; the
 * phase alone just says "Error". Other not-ready phases (Pending, Scanning,
 * Suspended) already say what is going on, so they report no problem.
 */
export function statusProblem(status?: { phase?: string; conditions?: StatusCondition[] }): StatusProblem | undefined {
  if (status?.phase !== 'Error') return undefined
  const ready = status.conditions?.find((condition) => condition.type === 'Ready')
  if (ready?.status !== 'False' || !ready.reason) return undefined
  return { reason: ready.reason, message: ready.message || undefined }
}
