import { useNavigate } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Compass, Database, FileCode, SquareTerminal, X } from 'lucide-react'
import { Fragment, useEffect, useRef, useState, type ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { useSendMessage } from '@/hooks/use-chat'
import { useMediaQuery } from '@/hooks/use-media-query'
import { cn } from '@/lib/utils'
import { useChatStore } from '@/stores/chat'
import { useUIStore } from '@/stores/ui'
import { DEMO_NAMESPACE } from '../shared'
import { toYaml } from './manifests'
import { stopsIn, TOUR_STEPS, type TourStep } from './steps'
import { useTourStore } from './store'

/**
 * From Tailwind's xl breakpoint the tour is a rail beside the page, below it a
 * panel under the page. Either way it takes its own space, so it never covers
 * the controls a step asks the visitor to use.
 */
const RAIL_MEDIA_QUERY = '(min-width: 1280px)'

// The sample data lives in one namespace, and a visitor may have switched away.
const switchToSampleNamespace = () => useUIStore.getState().setNamespace(DEMO_NAMESPACE)

function useTour() {
  const { step, goTo, close } = useTourStore()
  const navigate = useNavigate()
  const go = (index: number) => {
    goTo(index)
    const route = TOUR_STEPS[index]?.route
    if (route) {
      switchToSampleNamespace()
      void navigate(route)
    }
  }
  return { step, current: TOUR_STEPS[step], go, close }
}

function Progress({ step }: { step: TourStep }) {
  if (!step.part) return null
  const stops = stopsIn(step.part)
  const index = stops.indexOf(step)
  return (
    <div className="flex items-center gap-3">
      <span className="shrink-0 text-xs font-medium text-muted-foreground">
        {step.part === 'more' && 'More: '}Step {index + 1} of {stops.length}
      </span>
      <div className="flex flex-1 gap-1" aria-hidden="true">
        {stops.map((stop, i) => (
          <span key={stop.title} className={cn('h-1 flex-1 rounded-full', i <= index ? 'bg-primary' : 'bg-muted')} />
        ))}
      </div>
    </div>
  )
}

function StepBody({ step }: { step: TourStep }) {
  const navigate = useNavigate()
  const sendMessage = useSendMessage()
  const streaming = useChatStore((s) => s.isStreaming)
  const namespace = useUIStore((s) => s.namespace)
  const chatNamespace = useChatStore((s) => s.activeNamespace)
  // A prompt waits until both the page and Chat are on the sample namespace:
  // sendMessage posts to the namespace it was rendered with, and Chat clears
  // its transcript, in-flight turn included, when it moves between namespaces.
  // Each step mounts its own StepBody, so leaving the step drops a held prompt.
  const pendingPrompt = useRef<string | null>(null)
  useEffect(() => {
    const prompt = pendingPrompt.current
    if (prompt === null || namespace !== DEMO_NAMESPACE || chatNamespace !== DEMO_NAMESPACE) return
    pendingPrompt.current = null
    void sendMessage(prompt)
  }, [namespace, chatNamespace, sendMessage])
  return (
    <div className="space-y-3">
      {/* One live region, so a screen reader announces the points with the body. */}
      <div aria-live="polite" className="space-y-3 text-sm leading-relaxed text-muted-foreground">
        <p className="whitespace-pre-line">{step.body}</p>
        {step.points && (
          <ul className="list-disc space-y-1.5 pl-5 marker:text-primary">
            {step.points.map((point) => <li key={point}>{point}</li>)}
          </ul>
        )}
      </div>
      {step.prompts && (
        <div className="flex flex-col gap-2">
          {step.prompts.map((prompt) => (
            <Button
              key={prompt}
              variant="outline"
              size="sm"
              className="h-auto justify-start whitespace-normal py-1.5 text-left"
              disabled={streaming}
              onClick={() => {
                void navigate({ to: '/chat' })
                if (namespace === DEMO_NAMESPACE && chatNamespace === DEMO_NAMESPACE) {
                  void sendMessage(prompt)
                } else {
                  pendingPrompt.current = prompt
                  switchToSampleNamespace()
                }
              }}
            >
              {prompt}
            </Button>
          ))}
        </div>
      )}
      <StepSource key={step.title} step={step} />
    </div>
  )
}

/** Where the page's data comes from, and the same thing as YAML or from the CLI. */
function StepSource({ step }: { step: TourStep }) {
  const [open, setOpen] = useState<'yaml' | 'cli' | null>(null)
  const { manifest, note, cli } = step
  const toggle = (view: 'yaml' | 'cli') => setOpen(open === view ? null : view)
  if (!note && !manifest && !cli) return null
  return (
    <div className="space-y-2">
      {note && (
        <p className="flex gap-2 text-xs text-muted-foreground">
          <Database className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden="true" />
          {note}
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        {manifest && (
          <Button variant="outline" size="sm" aria-expanded={open === 'yaml'} onClick={() => toggle('yaml')}>
            <FileCode className="h-4 w-4" aria-hidden="true" />
            {open === 'yaml' ? 'Hide the YAML' : 'Show the YAML'}
          </Button>
        )}
        {cli && (
          <Button variant="outline" size="sm" aria-expanded={open === 'cli'} onClick={() => toggle('cli')}>
            <SquareTerminal className="h-4 w-4" aria-hidden="true" />
            {open === 'cli' ? 'Hide the CLI' : 'Show the CLI'}
          </Button>
        )}
      </div>
      {open === 'yaml' && manifest && (
        <SourceBlock label={`${manifest.kind} YAML`} caption={`${manifest.kind} ${manifest.metadata.name}, trimmed to the fields this stop mentions. You declare it with kubectl apply.`}>
          {toYaml(manifest)}
        </SourceBlock>
      )}
      {open === 'cli' && cli && (
        <SourceBlock label="CLI commands" caption="The same from the orka CLI, against a real cluster." wrap>
          {/* Long commands wrap between words with a hanging indent; each stays one line when copied. */}
          {cli.map((line) => (
            <span key={line} className={cn('block pl-[2ch] -indent-[2ch]', line.startsWith('#') && 'text-muted-foreground')}>
              {line.split(' ').map((word, index) => (
                <Fragment key={index}>
                  {index > 0 && ' '}
                  <span className="whitespace-nowrap">{word}</span>
                </Fragment>
              ))}
            </span>
          ))}
        </SourceBlock>
      )}
    </div>
  )
}

function SourceBlock({ label, caption, wrap, children }: { label: string; caption: string; wrap?: boolean; children: ReactNode }) {
  return (
    <>
      <p className="text-xs text-muted-foreground">{caption}</p>
      <pre
        tabIndex={0}
        role="region"
        aria-label={label}
        className={cn(
          'overflow-x-auto rounded-md bg-muted p-3 font-mono text-[11px] leading-relaxed',
          wrap && 'whitespace-pre-wrap',
        )}
      >
        {children}
      </pre>
    </>
  )
}

function StepNav() {
  const { step, go, close } = useTour()
  if (step === 0) {
    return (
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" size="sm" onClick={close}>Explore on my own</Button>
        <Button size="sm" onClick={() => go(1)}>Start the tour</Button>
      </div>
    )
  }
  const { checkpoint } = TOUR_STEPS[step]
  const last = step === TOUR_STEPS.length - 1
  return (
    <div className="flex justify-between gap-2">
      <Button variant="ghost" size="sm" onClick={() => go(step - 1)}>Back</Button>
      <div className="flex gap-2">
        {checkpoint && <Button variant="ghost" size="sm" onClick={close}>Finish</Button>}
        {last
          ? <Button size="sm" onClick={close}>Finish</Button>
          : <Button size="sm" onClick={() => go(step + 1)}>{checkpoint ? 'Keep going' : 'Next'}</Button>}
      </div>
    </div>
  )
}

function TourRail() {
  const { current, close } = useTour()
  return (
    <aside aria-label="Demo tour" className="flex w-80 shrink-0 flex-col border-l border-border bg-card/80 backdrop-blur-md">
      <div className="flex h-14 shrink-0 items-center justify-between border-b border-border px-4">
        <span className="flex items-center gap-2 text-sm font-semibold">
          <Compass className="h-4 w-4 text-primary" aria-hidden="true" />
          Demo tour
        </span>
        <Button variant="ghost" size="icon-sm" onClick={close} aria-label="Close the tour">
          <X className="h-4 w-4" />
        </Button>
      </div>
      <div className="flex-1 space-y-4 overflow-y-auto p-5">
        <Progress step={current} />
        <h2 className="text-base font-semibold">{current.title}</h2>
        <StepBody key={current.title} step={current} />
        <StepNav />
      </div>
    </aside>
  )
}

function TourSheet() {
  const { current, close } = useTour()
  const { collapsed, setCollapsed } = useTourStore()
  return (
    <aside aria-label="Demo tour" className="shrink-0 border-t border-border bg-card px-4 py-3 md:px-6">
      <div className="mx-auto max-w-3xl">
        <div className="flex items-center gap-2">
          <Compass className="h-4 w-4 shrink-0 text-primary" aria-hidden="true" />
          <h2 className="min-w-0 flex-1 truncate text-sm font-semibold">{current.title}</h2>
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={() => setCollapsed(!collapsed)}
            aria-label={collapsed ? 'Expand the tour' : 'Collapse the tour'}
            aria-expanded={!collapsed}
          >
            {collapsed ? <ChevronUp className="h-4 w-4" /> : <ChevronDown className="h-4 w-4" />}
          </Button>
          <Button variant="ghost" size="icon-sm" onClick={close} aria-label="Close the tour">
            <X className="h-4 w-4" />
          </Button>
        </div>
        {!collapsed && (
          <div className="mt-2 max-h-[45vh] space-y-3 overflow-y-auto">
            <Progress step={current} />
            <StepBody key={current.title} step={current} />
            <StepNav />
          </div>
        )}
      </div>
    </aside>
  )
}

/** The guided tour, placed by RootLayout in both slots; only the one for the current width renders. */
export function TourPanel({ slot }: { slot: 'rail' | 'bottom' }) {
  const open = useTourStore((s) => s.open)
  const wide = useMediaQuery(RAIL_MEDIA_QUERY)
  if (!open || (slot === 'rail') !== wide) return null
  return slot === 'rail' ? <TourRail /> : <TourSheet />
}

/** Sidebar entry that (re)starts the tour. */
export function TourLauncher({ collapsed, onSelect }: { collapsed: boolean; onSelect?: () => void }) {
  const start = useTourStore((s) => s.start)
  return (
    <div className="border-t border-border p-2">
      <button
        type="button"
        onClick={() => {
          start()
          onSelect?.()
        }}
        aria-label={collapsed ? 'Take the tour' : undefined}
        title={collapsed ? 'Take the tour' : undefined}
        className={cn(
          'flex w-full items-center gap-3 rounded-md px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground',
          collapsed && 'justify-center px-2',
        )}
      >
        <Compass className="h-4 w-4 shrink-0" />
        {!collapsed && <span>Take the tour</span>}
      </button>
    </div>
  )
}
