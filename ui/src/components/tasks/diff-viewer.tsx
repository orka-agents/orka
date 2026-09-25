import { ScrollArea } from '@/components/ui/scroll-area'

interface DiffViewerProps {
  diff: string
}

interface DiffLine {
  type: 'addition' | 'deletion' | 'context' | 'hunk-header' | 'file-header' | 'metadata'
  content: string
  oldLine?: number
  newLine?: number
}

function parseDiff(diff: string): DiffLine[] {
  const lines = diff.split('\n')
  const result: DiffLine[] = []
  let oldLine = 0
  let newLine = 0
  let oldLinesRemaining = 0
  let newLinesRemaining = 0

  for (const line of lines) {
    // Source lines can resemble file headers until the current hunk is complete.
    const inHunk = oldLinesRemaining > 0 || newLinesRemaining > 0
    if (
      line.startsWith('diff ') ||
      (!inHunk && (line.startsWith('--- ') || line.startsWith('+++ ')))
    ) {
      result.push({ type: 'file-header', content: line })
      oldLinesRemaining = 0
      newLinesRemaining = 0
    } else if (line.startsWith('@@')) {
      const match = line.match(/^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/)
      if (match) {
        oldLine = parseInt(match[1], 10)
        newLine = parseInt(match[3], 10)
        oldLinesRemaining = parseInt(match[2] ?? '1', 10)
        newLinesRemaining = parseInt(match[4] ?? '1', 10)
      }
      result.push({ type: 'hunk-header', content: line })
    } else if (line.startsWith('+')) {
      result.push({ type: 'addition', content: line, newLine })
      newLine++
      newLinesRemaining = Math.max(0, newLinesRemaining - 1)
    } else if (line.startsWith('-')) {
      result.push({ type: 'deletion', content: line, oldLine })
      oldLine++
      oldLinesRemaining = Math.max(0, oldLinesRemaining - 1)
    } else if (line.startsWith(' ')) {
      result.push({ type: 'context', content: line, oldLine, newLine })
      oldLine++
      newLine++
      oldLinesRemaining = Math.max(0, oldLinesRemaining - 1)
      newLinesRemaining = Math.max(0, newLinesRemaining - 1)
    } else {
      // Metadata such as "\ No newline at end of file" and "index ..." rows:
      // keep them visible, but never assign source line numbers or advance
      // counters. The empty item produced by a trailing patch newline is
      // dropped entirely.
      if (line === '') continue
      result.push({ type: 'metadata', content: line })
    }
  }

  return result
}

const lineStyles: Record<DiffLine['type'], string> = {
  addition: 'bg-status-succeeded-bg text-status-succeeded',
  deletion: 'bg-status-failed-bg text-status-failed',
  'hunk-header': 'bg-status-running-bg text-status-running',
  'file-header': 'bg-muted font-semibold',
  context: '',
  metadata: '',
}

export function DiffViewer({ diff }: DiffViewerProps) {
  if (!diff) {
    return <p className="text-sm text-muted-foreground">No diff available</p>
  }

  const lines = parseDiff(diff)

  return (
    <ScrollArea className="max-h-[500px] rounded-md border">
      <div className="font-mono text-xs" data-testid="diff-viewer">
        {lines.map((line, i) => (
          <div key={i} className={`flex ${lineStyles[line.type]}`}>
            <span className="w-10 shrink-0 select-none border-r px-1 text-right text-muted-foreground">
              {line.type === 'deletion' || line.type === 'context' ? line.oldLine : ''}
            </span>
            <span className="w-10 shrink-0 select-none border-r px-1 text-right text-muted-foreground">
              {line.type === 'addition' || line.type === 'context' ? line.newLine : ''}
            </span>
            <span className="flex-1 whitespace-pre-wrap px-2">{line.content}</span>
          </div>
        ))}
      </div>
    </ScrollArea>
  )
}
