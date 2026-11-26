import { useState } from 'react'
import { Search, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { removeTerm } from '@/lib/oversight'
import type { QueryTerm } from '@/schemas/oversight'

/**
 * Search modes. Only the deterministic query grammar exists. A future "ask"
 * mode (a natural-language question answered over the decisions) plugs in
 * here as a second mode with its own submit path; it is deliberately not
 * built, so nothing below may call a model.
 */
export type SearchMode = 'query' // | 'ask'

const WINDOW_OPTIONS = [
  { value: '', label: 'All time' },
  { value: '1h', label: 'Last hour' },
  { value: '24h', label: 'Last 24 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
] as const

interface SearchBoxProps {
  q: string
  window?: string
  /** The terms the server read the query as, echoed back. */
  terms?: QueryTerm[]
  /** The grammar's refusal (a 400), shown inline. */
  error?: string
  queryKeys?: string[]
  onSubmit: (q: string) => void
  onWindowChange: (window: string | undefined) => void
}

export function SearchBox({ q, window, terms, error, queryKeys, onSubmit, onWindowChange }: SearchBoxProps) {
  // The draft follows the URL whenever the URL's query changes (pivots,
  // removed chips, back/forward); typing only edits the draft.
  const [draft, setDraft] = useState({ text: q, seen: q })
  if (draft.seen !== q) setDraft({ text: q, seen: q })
  const mode: SearchMode = 'query'
  const [showHelp, setShowHelp] = useState(false)
  const shownTerms = terms ?? []

  return (
    <div className="space-y-2" data-search-mode={mode}>
      <form
        role="search"
        className="flex flex-col gap-2 sm:flex-row"
        onSubmit={(event) => {
          event.preventDefault()
          onSubmit(draft.text.trim())
        }}
      >
        <div className="relative flex-1">
          <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" aria-hidden="true" />
          <Input
            aria-label="Search oversight decisions"
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? 'oversight-search-error' : undefined}
            className="pl-8 font-mono"
            placeholder='try "push prod" or "token"'
            value={draft.text}
            onChange={(event) => setDraft({ text: event.target.value, seen: q })}
          />
        </div>
        <select
          aria-label="Time window"
          className="h-9 rounded-md border border-input bg-transparent px-2 text-sm dark:bg-input/30"
          value={window ?? ''}
          onChange={(event) => onWindowChange(event.target.value || undefined)}
        >
          {WINDOW_OPTIONS.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
        </select>
        <Button type="submit">Search</Button>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-label="Search help"
          aria-expanded={showHelp}
          aria-controls="oversight-search-help"
          onClick={() => setShowHelp((v) => !v)}
        >
          ?
        </Button>
        {q && (
          <Button type="button" variant="ghost" onClick={() => onSubmit('')}>Clear</Button>
        )}
      </form>
      {error && (
        <p id="oversight-search-error" role="alert" className="text-sm text-status-failed">{error}</p>
      )}
      {showHelp && (
        <div id="oversight-search-help" className="space-y-1 rounded-md border bg-muted/40 p-3 text-xs text-muted-foreground">
          <p>Words are matched as keywords against each action&apos;s redacted text; every word must match. &quot;Quoted phrases&quot; match as typed.</p>
          <p>
            Narrow with key:value terms, which all must match; list alternatives with | or , and use * as a wildcard,
            for example <span className="font-mono">bin:curl</span> or <span className="font-mono">task:build-*</span>.
          </p>
          {queryKeys && queryKeys.length > 0 && <p>Keys: <span className="font-mono">{queryKeys.join(' ')}</span></p>}
        </div>
      )}
      {shownTerms.length > 0 && (
        <ul className="flex flex-wrap gap-1.5" aria-label="Search terms">
          {shownTerms.map((term, index) => (
            <li key={`${term.key}:${term.value}:${index}`}>
              <span className="inline-flex items-center gap-1 rounded-md border bg-muted px-1.5 py-0.5 font-mono text-xs">
                {term.key === 'text' ? <>&ldquo;{term.value}&rdquo;</> : <>{term.key}:{term.value}</>}
                <button
                  type="button"
                  className="rounded hover:bg-accent focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                  aria-label={`Remove ${term.key === 'text' ? term.value : `${term.key}:${term.value}`}`}
                  onClick={() => onSubmit(removeTerm(shownTerms, index))}
                >
                  <X className="size-3" aria-hidden="true" />
                </button>
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
