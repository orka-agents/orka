import { useState, useRef, useEffect } from 'react'
import { Button } from '@/components/ui/button'
import { Send, Square } from 'lucide-react'
import { useChatStore } from '@/stores/chat'

interface ChatInputProps {
  onSend: (message: string) => void
  /** Blocks sending (for example while no Provider is selected). */
  disabled?: boolean
  /** Shown next to the composer when sending is blocked. */
  disabledHint?: string
  /** The composer, for a page that hands focus back to it. */
  textareaRef?: React.RefObject<HTMLTextAreaElement | null>
}

export function ChatInput({ onSend, disabled = false, disabledHint, textareaRef: pageTextareaRef }: ChatInputProps) {
  const [input, setInput] = useState('')
  const ownTextareaRef = useRef<HTMLTextAreaElement>(null)
  const textareaRef = pageTextareaRef ?? ownTextareaRef
  const isStreaming = useChatStore((s) => s.isStreaming)
  const stopRequested = useChatStore((s) => s.stopRequested)
  const requestStop = useChatStore((s) => s.requestStop)

  // Auto-resize textarea
  useEffect(() => {
    const el = textareaRef.current
    if (el) {
      el.style.height = 'auto'
      el.style.height = Math.min(el.scrollHeight, 200) + 'px'
    }
  }, [input, textareaRef])

  const handleSubmit = () => {
    const trimmed = input.trim()
    // The store, not the render snapshot: a second Enter in the same tick
    // must see the turn the first one just started.
    if (!trimmed || useChatStore.getState().isStreaming || disabled) return
    onSend(trimmed)
    setInput('')
  }

  // Pressing Stop disables it and then swaps it for Send, either of which
  // would drop focus to <body>; the composer takes it instead.
  const handleStop = () => {
    requestStop()
    textareaRef.current?.focus()
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSubmit()
    }
  }

  return (
    <div className="border-t border-border bg-card p-4">
      <div className="mx-auto flex max-w-3xl items-end gap-2">
        {/* The composer stays enabled while a reply streams so it keeps focus
            and the next message can be drafted; only sending is blocked. */}
        <textarea
          ref={textareaRef}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder="Message the meta agent..."
          rows={1}
          className="min-w-0 flex-1 resize-none rounded-xl border border-input bg-background px-4 py-3 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
        />
        {isStreaming ? (
          <Button
            size="icon"
            variant="outline"
            onClick={handleStop}
            disabled={stopRequested}
            aria-label={stopRequested ? 'Stopping and ending chat' : 'Stop and end this chat'}
            title={stopRequested ? 'Stopping…' : 'Stop and end this chat (its history is deleted)'}
            className="h-10 w-10 shrink-0 rounded-xl"
          >
            <Square className="h-4 w-4" />
          </Button>
        ) : (
          <Button
            size="icon"
            onClick={handleSubmit}
            disabled={!input.trim() || disabled}
            aria-label="Send message"
            title="Send message"
            className="h-10 w-10 shrink-0 rounded-xl"
          >
            <Send className="h-4 w-4" />
          </Button>
        )}
      </div>
      {disabled && disabledHint && (
        <p role="status" className="mx-auto mt-2 max-w-3xl text-xs text-muted-foreground">{disabledHint}</p>
      )}
    </div>
  )
}
