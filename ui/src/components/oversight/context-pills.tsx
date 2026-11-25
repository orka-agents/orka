import { useOversightConfig } from '@/hooks/use-oversight'

/** Operator-declared facts about this environment, shown as declared. */
export function ContextPills() {
  const { data: config } = useOversightConfig()
  const context = config?.context ?? []
  if (context.length === 0) return null
  return (
    <ul className="flex flex-wrap gap-1.5" aria-label="Declared environment">
      {context.map((label, i) => (
        <li
          key={`${label.name}-${i}`}
          title="Declared by the operator, not verified by Orka"
          className="rounded-full border border-border bg-muted/50 px-2 py-0.5 text-xs text-muted-foreground"
        >
          {label.name}: {label.value}
        </li>
      ))}
    </ul>
  )
}
