import { http, HttpResponse } from 'msw'
import { api, apiError, DEMO_NAMESPACE, query } from '../shared'
import { usageLedger } from '../data/usage'
import { OTHER_CATEGORIES, usageOther, usageReport, usageWork, type UsageFilter } from '../data/usage-report'

class BadRequest extends Error {}

/** Parses a `YYYY-MM-DD` (UTC midnight) or RFC 3339 query value. */
function parseDate(value: string | undefined, fallback: Date): Date {
  if (!value) return fallback
  const parsed = new Date(/^\d{4}-\d{2}-\d{2}$/.test(value) ? `${value}T00:00:00Z` : value)
  if (Number.isNaN(parsed.getTime())) throw new BadRequest(`invalid date ${JSON.stringify(value)}`)
  return parsed
}

/** The selection the controller derives from the query (see usageReportFilter). */
function usageFilter(request: Request, detail: boolean): UsageFilter {
  const now = new Date()
  const asOf = parseDate(query(request, 'asOf'), now)
  if (asOf.getTime() > now.getTime() + 1000) throw new BadRequest('asOf must not be in the future')
  const monthStart = new Date(Date.UTC(asOf.getUTCFullYear(), asOf.getUTCMonth(), 1))
  const from = parseDate(query(request, 'from'), detail ? new Date(0) : monthStart)
  const until = parseDate(query(request, 'until'), new Date(asOf.getTime() + 1))
  if (until.getTime() <= from.getTime()) throw new BadRequest('until must be after from')
  const kind = query(request, 'kind')
  if (kind && kind !== 'issue' && kind !== 'pull_request') throw new BadRequest('kind must be issue or pull_request')
  const teams = (query(request, 'teams') ?? '').split(',').map((team) => team.trim()).filter(Boolean)
  return {
    teams: teams.length > 0 ? teams : [query(request, 'namespace') ?? DEMO_NAMESPACE],
    repository: query(request, 'repository')?.trim() || undefined,
    model: query(request, 'model')?.trim() || undefined,
    kind,
    from,
    until,
    asOf,
  }
}

function usagePage(request: Request) {
  const read = (key: string, fallback: number, minimum: number) => {
    const raw = query(request, key)
    if (raw === undefined) return fallback
    const value = Number(raw)
    if (!Number.isInteger(value) || value < minimum) throw new BadRequest(`invalid usage ${key}`)
    return value
  }
  return { limit: Math.min(read('limit', 25, 1), 100), offset: read('offset', 0, 0) }
}

/** Runs a handler body, turning filter validation failures into 400s. */
function respond(body: () => Response) {
  try {
    return body()
  } catch (error) {
    if (error instanceof BadRequest) return apiError(400, error.message)
    throw error
  }
}

/** Usage and PR-outcome reporting. */
export const usageHandlers = [
  http.get(api('/usage'), ({ request }) =>
    respond(() => HttpResponse.json(usageReport(usageLedger, usageFilter(request, false), usagePage(request))))),

  http.get(api('/usage/work/:id'), ({ request, params }) =>
    respond(() => {
      const result = usageWork(usageLedger, usageFilter(request, true), String(params.id))
      return result ? HttpResponse.json(result) : apiError(404, 'work request not found')
    })),

  http.get(api('/usage/other/:category'), ({ request, params }) =>
    respond(() => {
      const category = String(params.category)
      if (!(OTHER_CATEGORIES as readonly string[]).includes(category)) return apiError(404, 'usage category not found')
      const result = usageOther(usageLedger, usageFilter(request, false), category, usagePage(request))
      return result ? HttpResponse.json(result) : apiError(404, 'usage category not found')
    })),
]
