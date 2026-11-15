import { Fragment, useEffect, useMemo, useState, type ReactNode } from 'react'
import { toast } from 'sonner'
import { Copy } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { displayedTaskResource } from '@/lib/task-resource'
import type { Task } from '@/schemas/task'

// Theme colors that already have dark-mode values; there are no code tokens.
const TOKEN = {
  key: 'text-primary',
  string: 'text-status-succeeded',
  number: 'text-status-pending',
  literal: 'text-type-ai',
  punctuation: 'text-muted-foreground',
}

const KEY_LINE = /^(\s*)(- )?("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^\s#'"-][^:#]*?|-[^\s:#][^:#]*?):(?=\s|$)(.*)$/
const LIST_ITEM = /^(\s*)- (.*)$/
// A block scalar header: `|` or `>`, then a chomping sign and indent digit in either order (`|-`, `|2-`).
const BLOCK_INDICATOR = /^[|>](?:[1-9][-+]?|[-+][1-9]?)?$/

function valueToken(value: string): keyof typeof TOKEN {
  if (/^(null|~|true|false)$/.test(value)) return 'literal'
  if (/^-?\d+(\.\d+)?([eE][-+]?\d+)?$/.test(value)) return 'number'
  if (BLOCK_INDICATOR.test(value) || value === '[]' || value === '{}') return 'punctuation'
  return 'string'
}

function Value({ text }: { text: string }) {
  const value = text.trim()
  if (!value) return <>{text}</>
  const lead = text.slice(0, text.indexOf(value))
  return <>{lead}<span className={TOKEN[valueToken(value)]}>{value}</span></>
}

/**
 * Colors the YAML that the yaml library writes: keys, scalars by type, list
 * dashes, and block scalars (`|`, `>`), whose lines are all text.
 */
function highlightYaml(text: string): ReactNode[] {
  let blockIndent: number | null = null
  const lines = text.split('\n')
  return lines.map((line, index) => {
    const indent = line.length - line.trimStart().length
    let content: ReactNode
    if (blockIndent !== null && (line.trim() === '' || indent > blockIndent)) {
      content = <span className={TOKEN.string}>{line}</span>
    } else {
      blockIndent = null
      const keyed = KEY_LINE.exec(line)
      const item = keyed ? null : LIST_ITEM.exec(line)
      if (keyed) {
        const [, space, dash, key, rest] = keyed
        if (BLOCK_INDICATOR.test(rest.trim())) blockIndent = space.length + (dash ? 2 : 0)
        content = (
          <>
            {space}
            {dash && <span className={TOKEN.punctuation}>{dash}</span>}
            <span className={TOKEN.key}>{key}</span>
            <span className={TOKEN.punctuation}>:</span>
            <Value text={rest} />
          </>
        )
      } else if (item) {
        const [, space, rest] = item
        content = <>{space}<span className={TOKEN.punctuation}>- </span><Value text={rest} /></>
      } else {
        // A long scalar the writer folded onto the next line.
        content = <span className={TOKEN.string}>{line}</span>
      }
    }
    return <Fragment key={index}>{content}{index < lines.length - 1 && '\n'}</Fragment>
  })
}

type Stringify = (value: unknown) => string

const loadYaml = () => import('yaml').then((yaml): Stringify => yaml.stringify)

/** The Task resource as YAML. The yaml library loads only when this view opens. */
export function TaskYaml({ task, load = loadYaml }: { task: Task; load?: () => Promise<Stringify> }) {
  const [stringify, setStringify] = useState<Stringify | null>(null)
  // A failed chunk load (a redeploy, a dropped connection) offers a retry
  // instead of a skeleton that never resolves.
  const [failed, setFailed] = useState(false)
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    let mounted = true
    load().then(
      (loaded) => {
        if (mounted) setStringify(() => loaded)
      },
      () => {
        if (mounted) setFailed(true)
      },
    )
    return () => {
      mounted = false
    }
  }, [load, attempt])
  const text = useMemo(() => stringify?.(displayedTaskResource(task)) ?? null, [stringify, task])

  const copy = () => {
    if (!text || !navigator.clipboard?.writeText) return
    void navigator.clipboard
      .writeText(text)
      .then(() => toast.success('Copied the Task YAML'))
      .catch(() => toast.error('Failed to copy to clipboard'))
  }

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <CardTitle>Task resource</CardTitle>
        <Button variant="outline" size="sm" onClick={copy} disabled={!text}>
          <Copy className="h-4 w-4" aria-hidden="true" /> Copy
        </Button>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-xs text-muted-foreground">
          Credentials show as (hidden): environment values, the credential parts of URLs, and any label, annotation or text that looks like it holds one. kubectl&apos;s annotations, which can embed the whole spec, are left out.
        </p>
        {failed ? (
          <div role="alert" className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-destructive/40 p-3 text-sm">
            <span>The YAML view could not load. Check your connection and try again.</span>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                setFailed(false)
                setAttempt((n) => n + 1)
              }}
            >
              Retry
            </Button>
          </div>
        ) : text === null ? (
          <Skeleton className="h-64 w-full" />
        ) : (
          <pre
            tabIndex={0}
            role="region"
            aria-label="Task YAML"
            className="overflow-x-auto rounded-md bg-muted p-4 font-mono text-xs leading-relaxed"
          >
            {highlightYaml(text.trimEnd())}
          </pre>
        )}
      </CardContent>
    </Card>
  )
}
