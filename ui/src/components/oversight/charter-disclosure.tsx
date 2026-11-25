import { useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { useOversightConfig } from '@/hooks/use-oversight'
import { charterName, isDemoCharter, orderedBounds } from '@/lib/oversight'
import { CharterPanel } from './charter-panel'

/**
 * The charter as one collapsed line under the incidents:
 * "Demo charter · Rule 1 highlighted · 2 broken". The Charter tab has the rest.
 */
export function CharterDisclosure({ window, broken, onPivot, onOpenBound }: {
  window: string
  broken: number
  onPivot: (q: string) => void
  onOpenBound?: (id: string) => void
}) {
  const { data: config } = useOversightConfig()
  const [open, setOpen] = useState(false)
  const charters = config?.charters ?? []
  if (charters.length === 0) return null
  const first = charters[0]
  const highlighted = orderedBounds(first).filter((b) => b.highlight)
  const name = isDemoCharter(first, config) ? 'Demo charter' : charterName(first)
  const detail = highlighted.length > 0
    ? `Rule ${highlighted.map((b) => b.number).join(', ')} highlighted`
    : `${first.bounds?.length ?? 0} rules`
  return (
    <div className="space-y-2">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
      >
        {open ? <ChevronDown className="size-4" aria-hidden="true" /> : <ChevronRight className="size-4" aria-hidden="true" />}
        {name} · {detail} · {broken} broken{charters.length > 1 ? ` · ${charters.length} charters` : ''}
      </button>
      {open && <CharterPanel window={window} onPivot={onPivot} onOpenBound={onOpenBound} />}
    </div>
  )
}
