import { useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { useOversightConfig } from '@/hooks/use-oversight'
import { charterName, isDemoCharter, orderedBounds } from '@/lib/oversight'
import { CharterPanel } from './charter-panel'

/**
 * The charter, one click away: "Demo charter · Rule 1 highlighted". It
 * starts open when an operator highlighted a rule.
 */
export function CharterDisclosure({ window, onPivot }: { window: string; onPivot: (q: string) => void }) {
  const { data: config } = useOversightConfig()
  const [override, setOverride] = useState<boolean | null>(null)
  const charters = config?.charters ?? []
  if (charters.length === 0) return null
  const first = charters[0]
  const highlighted = orderedBounds(first).filter((b) => b.highlight)
  const open = override ?? highlighted.length > 0
  const name = isDemoCharter(first, config) ? 'Demo charter' : charterName(first)
  const detail = highlighted.length > 0
    ? `Rule ${highlighted.map((b) => b.number).join(', ')} highlighted`
    : `${first.bounds?.length ?? 0} rules`
  return (
    <div className="space-y-2">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOverride(!open)}
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
      >
        {open ? <ChevronDown className="size-4" aria-hidden="true" /> : <ChevronRight className="size-4" aria-hidden="true" />}
        {name} · {detail}{charters.length > 1 ? ` · ${charters.length} charters` : ''}
      </button>
      {open && <CharterPanel window={window} onPivot={onPivot} />}
    </div>
  )
}
