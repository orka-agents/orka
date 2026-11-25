import { toast } from 'sonner'
import { useOversightConfig } from '@/hooks/use-oversight'
import { findCharterRule, charterName } from '@/lib/oversight'
import { useOversightAlerts } from '@/stores/oversight-alerts'
import { useUIStore } from '@/stores/ui'
import type { DecisionView } from '@/schemas/oversight'

/**
 * Returns the handler for streamed decisions that raises a charter alert: a
 * toast and an entry in the session's alert list. Only an against-charter
 * decision with a finding alerts, once per decision, and never one that was
 * already on screen from the initial page load. Risky and
 * scorer-unavailable decisions stay rows only. The wording is audit-only:
 * nothing is held.
 */
export function useCharterAlertHandler(onOpen: (id: string) => void, initialIDs: Set<string>) {
  const { data: config } = useOversightConfig()
  const namespace = useUIStore((s) => s.namespace)
  const add = useOversightAlerts((s) => s.add)
  return (decision: DecisionView) => {
    if (decision.signal !== 'against_charter') return
    const finding = decision.charterFindings?.[0]
    if (!finding || initialIDs.has(decision.id)) return
    const rule = findCharterRule(config, decision.charterRevision, finding.boundID)
    const who = decision.agentName || decision.taskName
    const sentence = rule?.bound.sentence ?? finding.sentence
    const isNew = add({
      decisionID: decision.id,
      namespace,
      taskName: decision.taskName,
      agentName: decision.agentName,
      observedAt: decision.observedAt,
      rule: rule ? `Rule ${rule.bound.number}` : `Rule ${finding.boundID}`,
      charter: rule ? charterName(rule.charter) : decision.charterRevision,
      sentence,
    })
    if (!isNew) return
    toast(`Charter rule ${rule ? rule.bound.number : finding.boundID} broken by ${who}, would pause (audit mode)`, {
      description: sentence,
      action: { label: 'Inspect', onClick: () => onOpen(decision.id) },
    })
  }
}
