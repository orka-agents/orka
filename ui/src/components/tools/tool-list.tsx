import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { toast } from 'sonner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ManifestEditor } from '@/components/ui/manifest-editor'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { useCreateTool, useToolList } from '@/hooks/use-tools'
import { useUIStore } from '@/stores/ui'
import type { ToolSpec } from '@/schemas/tool'

// Starter manifest for a custom HTTP tool (the examples/tavily shape).
const toolTemplate = {
  name: 'my-tool',
  spec: {
    description: 'What the model should know this tool does',
    parameters: {
      type: 'object',
      properties: {
        query: { type: 'string', description: 'Search query' },
      },
      required: ['query'],
    },
    http: {
      url: 'https://api.example.com/search',
      method: 'POST',
      timeout: '10s',
      authSecretRef: { name: 'my-tool-credentials' },
      authInject: 'header',
    },
  },
}

export function ToolList() {
  const { data, isLoading } = useToolList()
  const createTool = useCreateTool()
  const namespace = useUIStore((s) => s.namespace)
  const [creating, setCreating] = useState(false)

  return (
    <div className="space-y-4">
      <PageHeader
        title="Tools"
        description="Available tools for AI agents"
        action={
          <Button onClick={() => setCreating(true)}>
            <Plus className="mr-2 h-4 w-4" />
            New tool
          </Button>
        }
      />
      <ManifestEditor
        open={creating}
        onOpenChange={setCreating}
        title="New tool"
        description="Define a Tool CR: HTTP execution or an MCP substrate actor, with JSON-Schema parameters the model sees."
        initialValue={toolTemplate}
        submitLabel="Create tool"
        pending={createTool.isPending}
        onSubmit={async (manifest) => {
          const name = typeof manifest.name === 'string' ? manifest.name : ''
          if (!name) throw new Error('name is required at the top level')
          const spec = manifest.spec as ToolSpec | undefined
          if (!spec) throw new Error('spec is required')
          await createTool.mutateAsync({ name, namespace, spec })
          toast.success(`Tool ${name} created`)
          setCreating(false)
        }}
      />
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Namespace</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Description</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: 5 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 5 }).map((_, j) => (
                    <TableCell key={j}><Skeleton className="h-4 w-20" /></TableCell>
                  ))}
                </TableRow>
              ))
            ) : (data?.items ?? []).length === 0 ? (
              <TableRow>
                <TableCell colSpan={5} className="text-center text-muted-foreground py-8">
                  No tools found.
                </TableCell>
              </TableRow>
            ) : (
              (data?.items ?? []).map((tool) => (
                <TableRow key={tool.name}>
                  <TableCell>
                    <Link to="/tools/$toolName" params={{ toolName: tool.name }} className="font-medium hover:underline">
                      {tool.name}
                    </Link>
                  </TableCell>
                  <TableCell>{tool.namespace ?? '-'}</TableCell>
                  <TableCell>
                    <Badge variant={tool.builtin ? 'default' : 'secondary'}>
                      {tool.builtin ? 'Built-in' : 'Custom'}
                    </Badge>
                  </TableCell>
                  <TableCell className="max-w-xs truncate">{tool.description}</TableCell>
                  <TableCell>
                    {tool.builtin ? (
                      <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Available</Badge>
                    ) : tool.available ? (
                      <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Available</Badge>
                    ) : (
                      <Badge className="bg-status-failed-bg text-status-failed" variant="secondary">Unavailable</Badge>
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}
