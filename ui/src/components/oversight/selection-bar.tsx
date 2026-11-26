import { useMemo, useState } from 'react'
import { Copy, FilePlus2 } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { useOversightBulkFeedback, useOversightConfig } from '@/hooks/use-oversight'
import { FEEDBACK_LABEL_TEXT, draftConcernYaml } from '@/lib/oversight'
import { OVERSIGHT_FEEDBACK_LABELS, type DecisionView, type OversightFeedbackLabel } from '@/schemas/oversight'

const DEFAULT_MAX_BULK = 200

interface SelectionBarProps {
  selected: DecisionView[]
  /** Every row in the result set, for not_examples in a concern draft. */
  resultSet: DecisionView[]
  query: string
  onClear: () => void
}

/** Actions on selected search or similar results: bulk labels and a concern draft. */
export function SelectionBar({ selected, resultSet, query, onClear }: SelectionBarProps) {
  const { data: config } = useOversightConfig()
  const maxBulk = config?.maxBulkFeedback || DEFAULT_MAX_BULK
  const bulk = useOversightBulkFeedback()
  const [note, setNote] = useState('')
  const [draftOpen, setDraftOpen] = useState(false)
  if (selected.length === 0) return null
  const tooMany = selected.length > maxBulk

  const label = (value: OversightFeedbackLabel) => {
    bulk.mutate(
      { ids: selected.map((d) => d.id), label: value, note: note.trim() || undefined },
      {
        onSuccess: (response) => {
          toast.success(`Labelled ${response.recorded} ${response.recorded === 1 ? 'action' : 'actions'} ${FEEDBACK_LABEL_TEXT[value].toLowerCase()}`)
          setNote('')
          onClear()
        },
        onError: (error) => toast.error(`Could not record the labels: ${error instanceof Error ? error.message : 'request failed'}`),
      },
    )
  }

  return (
    <div
      role="region"
      aria-label="Selection actions"
      className="sticky bottom-0 z-10 flex flex-col gap-2 rounded-md border bg-card/95 p-3 shadow-lg backdrop-blur sm:flex-row sm:flex-wrap sm:items-center"
    >
      <span className="text-sm font-medium tabular-nums">{selected.length} selected</span>
      <Input
        aria-label="Note for all selected (optional)"
        placeholder="One note for all (optional)"
        className="h-8 sm:w-56"
        value={note}
        maxLength={1000}
        onChange={(event) => setNote(event.target.value)}
      />
      <div className="flex flex-wrap gap-1.5">
        {OVERSIGHT_FEEDBACK_LABELS.map((value) => (
          <Button key={value} size="sm" variant="outline" disabled={tooMany || bulk.isPending} onClick={() => label(value)}>
            Label {FEEDBACK_LABEL_TEXT[value].toLowerCase()}
          </Button>
        ))}
      </div>
      <Button size="sm" variant="ghost" onClick={() => setDraftOpen(true)}>
        <FilePlus2 aria-hidden="true" /> Draft concern from this search
      </Button>
      <Button size="sm" variant="ghost" onClick={onClear}>Clear</Button>
      {tooMany && <p className="w-full text-xs text-status-failed" role="status">Label at most {maxBulk} actions at once.</p>}
      <DraftConcernDialog open={draftOpen} onOpenChange={setDraftOpen} query={query} selected={selected} resultSet={resultSet} />
    </div>
  )
}

function DraftConcernDialog({ open, onOpenChange, query, selected, resultSet }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  query: string
  selected: DecisionView[]
  resultSet: DecisionView[]
}) {
  const yaml = useMemo(() => (open ? draftConcernYaml(query, selected, resultSet) : ''), [open, query, selected, resultSet])
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(yaml)
      toast.success('Copied the draft')
    } catch {
      toast.error('Could not copy; select the text instead')
    }
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Draft concern</DialogTitle>
          <DialogDescription>
            Built in your browser from this search: it changes nothing on the server. Examples come from the selected actions&apos;
            redacted narratives, not_examples from actions labelled false alarms.
          </DialogDescription>
        </DialogHeader>
        <textarea
          readOnly
          aria-label="Concern YAML draft"
          value={yaml}
          className="h-72 w-full resize-y rounded-md border bg-muted p-3 font-mono text-xs"
        />
        <DialogFooter showCloseButton>
          <Button onClick={copy}><Copy aria-hidden="true" /> Copy YAML</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
