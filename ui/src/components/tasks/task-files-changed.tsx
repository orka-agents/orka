import { FileText } from 'lucide-react'

interface TaskFilesChangedProps {
  files: string[]
}

export function TaskFilesChanged({ files }: TaskFilesChangedProps) {
  if (!files.length) {
    return <p className="text-sm text-muted-foreground">No files changed</p>
  }

  return (
    <ul className="space-y-1" data-testid="files-changed">
      {files.map((file) => (
        <li key={file} className="flex items-center gap-2 text-sm font-mono">
          <FileText className="size-4 shrink-0 text-muted-foreground" />
          {file}
        </li>
      ))}
    </ul>
  )
}
