import { useMemo, useState } from 'react'
import { dump, load } from 'js-yaml'

import { Button } from '@/components/ui/button'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'

interface ManifestEditorProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  /** Object serialized to YAML when the dialog opens. */
  initialValue: unknown
  submitLabel: string
  pending?: boolean
  /** Receives the parsed YAML document. Throw or reject to keep the dialog open. */
  onSubmit: (value: Record<string, unknown>) => Promise<void> | void
}

// Shared spec editor for resources whose full schema is too deep for a form.
// The server stays the source of validation truth; this parses YAML locally
// and surfaces parse/submit errors inline.
export function ManifestEditor({
  open,
  onOpenChange,
  title,
  description,
  initialValue,
  submitLabel,
  pending,
  onSubmit,
}: ManifestEditorProps) {
  const initialText = useMemo(() => {
    try {
      return dump(initialValue ?? {}, { lineWidth: 100, noRefs: true })
    } catch {
      return ''
    }
  }, [initialValue])
  // Guarded render-time adjustment (not an effect): the editor re-seeds from
  // initialText each time the dialog transitions to open, discarding edits
  // from the previous session.
  const [editor, setEditor] = useState<{ openedWith: string | null; text: string; error: string | null }>({
    openedWith: null,
    text: initialText,
    error: null,
  })
  if (open && editor.openedWith === null) {
    setEditor({ openedWith: initialText, text: initialText, error: null })
  } else if (!open && editor.openedWith !== null) {
    setEditor({ openedWith: null, text: '', error: null })
  }
  const text = editor.text
  const error = editor.error
  const setText = (value: string) => setEditor((state) => ({ ...state, text: value }))
  const setError = (value: string | null) => setEditor((state) => ({ ...state, error: value }))

  const handleSubmit = async () => {
    let parsed: unknown
    try {
      parsed = load(text)
    } catch (parseError) {
      setError(parseError instanceof Error ? parseError.message : 'Invalid YAML')
      return
    }
    if (parsed === null || parsed === undefined || typeof parsed !== 'object' || Array.isArray(parsed)) {
      setError('The manifest must be a YAML mapping (key: value pairs).')
      return
    }
    try {
      await onSubmit(parsed as Record<string, unknown>)
      setError(null)
    } catch (submitError) {
      setError(submitError instanceof Error ? submitError.message : 'Submit failed')
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description && <DialogDescription>{description}</DialogDescription>}
        </DialogHeader>
        <textarea
          value={text}
          onChange={(event) => setText(event.target.value)}
          rows={18}
          spellCheck={false}
          aria-label="Manifest YAML"
          className="w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs leading-5 shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring"
        />
        {error && (
          <p role="alert" className="whitespace-pre-wrap break-words text-sm text-status-failed">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button onClick={handleSubmit} disabled={pending}>
            {pending ? 'Saving…' : submitLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
