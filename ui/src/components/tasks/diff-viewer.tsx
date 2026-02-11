import { ScrollArea } from '@/components/ui/scroll-area'

interface DiffViewerProps {
  diff: string
}

interface DiffLine {
  type: 'addition' | 'deletion' | 'context' | 'hunk-header' | 'file-header'
  content: string
  oldLine?: number
  newLine?: number
}

function parseDiff(diff: string): DiffLine[] {
  const lines = diff.split('\n')
  const result: DiffLine[] = []
  let oldLine = 0
  let newLine = 0

  for (const line of lines) {
    if (line.startsWith('--- ') || line.startsWith('+++ ') || line.startsWith('diff ')) {
      result.push({ type: 'file-header', content: line })
    } else if (line.startsWith('@@')) {
      const match = line.match(/@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
      if (match) {
        oldLine = parseInt(match[1], 10)
        newLine = parseInt(match[2], 10)
      }
      result.push({ type: 'hunk-header', content: line })
    } else if (line.startsWith('+')) {
      result.push({ type: 'addition', content: line, newLine })
      newLine++
    } else if (line.startsWith('-')) {
      result.push({ type: 'deletion', content: line, oldLine })
      oldLine++
    } else {
      result.push({ type: 'context', content: line, oldLine, newLine })
      oldLine++
      newLine++
    }
  }

  return result
}

const lineStyles: Record<DiffLine['type'], string> = {
  addition: 'bg-green-100 dark:bg-green-900/40 text-green-900 dark:text-green-200',
  deletion: 'bg-red-100 dark:bg-red-900/40 text-red-900 dark:text-red-200',
  'hunk-header': 'bg-blue-50 dark:bg-blue-900/30 text-blue-700 dark:text-blue-300',
  'file-header': 'bg-muted font-semibold',
  context: '',
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
