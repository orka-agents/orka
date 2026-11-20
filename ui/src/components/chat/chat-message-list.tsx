import { useEffect, useRef } from 'react'
import { ScrollArea } from '@/components/ui/scroll-area'
import { ChatMessage } from './chat-message'
import { useChatStore } from '@/stores/chat'
import { Loader2 } from 'lucide-react'
import { OrcaMark } from '@/components/ui/orca-mark'

// Within this distance of the bottom the reader is following the stream.
const NEAR_BOTTOM_PX = 80

// The transcript scrolls inside its own viewport (not the page), so the
// header stays put and only this element is ever scrolled.
function scrollViewport(root: HTMLElement | null) {
  return root?.querySelector<HTMLElement>('[data-slot="scroll-area-viewport"]') ?? null
}

export function ChatMessageList() {
  const messages = useChatStore((s) => s.messages)
  const isStreaming = useChatStore((s) => s.isStreaming)
  const scrollAreaRef = useRef<HTMLDivElement>(null)
  // Whether the reader was at the bottom as of their last scroll; content
  // growth alone must not change it, so it is tracked from scroll events.
  const followingRef = useRef(true)
  const hasMessages = messages.length > 0

  useEffect(() => {
    const el = scrollViewport(scrollAreaRef.current)
    if (!el) return
    const onScroll = () => {
      followingRef.current = el.scrollHeight - el.scrollTop - el.clientHeight <= NEAR_BOTTOM_PX
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [hasMessages])

  useEffect(() => {
    const el = scrollViewport(scrollAreaRef.current)
    if (!el) return
    // A reader who scrolled up keeps their place; sending a message of their
    // own always brings the transcript back to the bottom.
    if (followingRef.current || messages[messages.length - 1]?.role === 'user') {
      el.scrollTop = el.scrollHeight
      followingRef.current = true
    }
  }, [messages, isStreaming])

  if (!hasMessages) {
    return (
      <div className="flex flex-1 items-center justify-center">
        <div className="text-center">
          <OrcaMark className="mx-auto h-12 w-12" />
          <h2 className="mt-4 text-lg font-semibold">Orka Meta Agent</h2>
          <p className="mt-1 max-w-sm text-sm text-muted-foreground">
            Chat with the orchestrator to create and manage tasks, agents, and tools using natural language.
          </p>
        </div>
      </div>
    )
  }

  // Radix wraps the viewport's content in a `display: table` box that grows
  // to its widest unbreakable line, so one long tool argument would widen
  // and clip the whole transcript; a block wrapper keeps it to the viewport.
  return (
    <ScrollArea className="min-h-0 flex-1" viewportClassName="[&>div]:block!" ref={scrollAreaRef}>
      <div className="mx-auto max-w-3xl space-y-1 px-4 py-4">
        {messages.map((msg) => (
          <ChatMessage key={msg.id} message={msg} />
        ))}
        {isStreaming && (
          <div className="flex items-center gap-2 px-12 py-2 text-xs text-muted-foreground">
            <Loader2 className="h-3 w-3 animate-spin" />
            <span>Thinking...</span>
          </div>
        )}
      </div>
    </ScrollArea>
  )
}
