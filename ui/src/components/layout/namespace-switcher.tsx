import { useMemo, useState } from 'react'
import { Boxes, Check, ChevronsUpDown } from 'lucide-react'

import { cn } from '@/lib/utils'
import { useUIStore } from '@/stores/ui'
import { useWhoAmI } from '@/hooks/use-whoami'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

const BASE_SUGGESTIONS = ['default', 'orka-system']

// Free-text namespace switcher. Kubernetes offers no namespace-list endpoint
// through the Orka API, so this combines the caller's own namespace, recent
// choices, and common defaults with direct entry.
export function NamespaceSwitcher() {
  const { namespace, namespaceHistory, setNamespace } = useUIStore()
  const { data: whoami } = useWhoAmI()
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState('')

  const suggestions = useMemo(() => {
    const seen = new Set<string>()
    const ordered: string[] = []
    for (const candidate of [
      namespace,
      whoami?.namespace ?? '',
      ...(namespaceHistory ?? []),
      ...BASE_SUGGESTIONS,
    ]) {
      const value = candidate.trim()
      if (!value || seen.has(value)) continue
      seen.add(value)
      ordered.push(value)
    }
    const filter = draft.trim().toLowerCase()
    return filter ? ordered.filter((value) => value.toLowerCase().includes(filter)) : ordered
  }, [namespace, whoami?.namespace, namespaceHistory, draft])

  const apply = (value: string) => {
    const trimmed = value.trim()
    if (!trimmed) return
    setNamespace(trimmed)
    setDraft('')
    setOpen(false)
  }

  return (
    <Popover open={open} onOpenChange={(next) => { setOpen(next); if (!next) setDraft('') }}>
      <PopoverTrigger asChild>
        <Button
          variant="outline"
          role="combobox"
          aria-expanded={open}
          aria-label="Switch namespace"
          className="w-52 justify-between font-normal"
        >
          <span className="flex min-w-0 items-center gap-2">
            <Boxes className="h-4 w-4 shrink-0 text-muted-foreground" />
            <span className="truncate">{namespace}</span>
          </span>
          <ChevronsUpDown className="h-4 w-4 shrink-0 text-muted-foreground" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 p-2">
        <form
          onSubmit={(event) => {
            event.preventDefault()
            apply(draft || namespace)
          }}
        >
          <Input
            autoFocus
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder="Type a namespace…"
            aria-label="Namespace"
            className="mb-2 h-8"
          />
        </form>
        <ul className="max-h-56 space-y-0.5 overflow-y-auto" aria-label="Namespace suggestions">
          {suggestions.map((value) => (
            <li key={value}>
              <button
                type="button"
                onClick={() => apply(value)}
                className={cn(
                  'flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-sm transition-colors hover:bg-accent hover:text-accent-foreground',
                  value === namespace && 'font-medium',
                )}
              >
                <Check className={cn('h-3.5 w-3.5', value === namespace ? 'opacity-100' : 'opacity-0')} />
                <span className="truncate">{value}</span>
                {value === whoami?.namespace && (
                  <span className="ml-auto text-xs text-muted-foreground">token</span>
                )}
              </button>
            </li>
          ))}
          {suggestions.length === 0 && (
            <li className="px-2 py-1.5 text-sm text-muted-foreground">
              Press Enter to use “{draft.trim()}”
            </li>
          )}
        </ul>
      </PopoverContent>
    </Popover>
  )
}
