import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useCreateTask } from '@/hooks/use-tasks'
import { useAgentList } from '@/hooks/use-agents'
import { useUIStore } from '@/stores/ui'
import { toast } from 'sonner'

export function TaskCreateForm() {
  const navigate = useNavigate()
  const createTask = useCreateTask()
  const { data: agentsData } = useAgentList()
  const namespace = useUIStore((s) => s.namespace)

  const [name, setName] = useState('')
  const [type, setType] = useState<string>('container')
  const [image, setImage] = useState('')
  const [command, setCommand] = useState('')
  const [provider, setProvider] = useState('')
  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [agentRef, setAgentRef] = useState('')

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const body: Record<string, unknown> = { name, namespace, type }

    if (type === 'container') {
      body.image = image
      if (command) body.command = command.split(' ')
    } else if (type === 'ai') {
      body.ai = { provider, model, prompt }
    } else if (type === 'agent') {
      body.agentRef = { name: agentRef }
      body.prompt = prompt
    }

    try {
      await createTask.mutateAsync(body)
      toast.success('Task created')
      navigate({ to: '/tasks' })
    } catch (err) {
      toast.error(`Failed to create task: ${err instanceof Error ? err.message : 'Unknown error'}`)
    }
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-3xl font-bold tracking-tight">Create Task</h1>
        <p className="text-muted-foreground">Create a new task for execution</p>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Task Configuration</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium">Name</label>
                <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="my-task" required />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Type</label>
                <Select value={type} onValueChange={setType}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="container">Container</SelectItem>
                    <SelectItem value="ai">AI</SelectItem>
                    <SelectItem value="agent">Agent</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            {type === 'container' && (
              <div className="space-y-4">
                <div className="space-y-2">
                  <label className="text-sm font-medium">Image</label>
                  <Input value={image} onChange={(e) => setImage(e.target.value)} placeholder="alpine:latest" required />
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Command</label>
                  <Input value={command} onChange={(e) => setCommand(e.target.value)} placeholder="echo hello" />
                </div>
              </div>
            )}

            {type === 'ai' && (
              <div className="space-y-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Provider</label>
                    <Select value={provider} onValueChange={setProvider}>
                      <SelectTrigger><SelectValue placeholder="Select provider" /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="anthropic">Anthropic</SelectItem>
                        <SelectItem value="openai">OpenAI</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <label className="text-sm font-medium">Model</label>
                    <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="claude-sonnet-4-20250514" />
                  </div>
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Prompt</label>
                  <textarea
                    className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    value={prompt}
                    onChange={(e) => setPrompt(e.target.value)}
                    placeholder="Enter your prompt..."
                    required
                  />
                </div>
              </div>
            )}

            {type === 'agent' && (
              <div className="space-y-4">
                <div className="space-y-2">
                  <label className="text-sm font-medium">Agent Reference</label>
                  <Select value={agentRef} onValueChange={setAgentRef}>
                    <SelectTrigger><SelectValue placeholder="Select an agent..." /></SelectTrigger>
                    <SelectContent>
                      {(agentsData?.items ?? []).map((a) => (
                        <SelectItem key={a.metadata.name} value={a.metadata.name}>
                          {a.metadata.name}
                          {a.spec.model?.name ? ` (${a.spec.model.name})` : ''}
                          {a.spec.runtime ? ` (${a.spec.runtime.type} runtime)` : ''}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-medium">Prompt</label>
                  <textarea
                    className="flex min-h-24 w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    value={prompt}
                    onChange={(e) => setPrompt(e.target.value)}
                    placeholder="Enter your prompt..."
                    required
                  />
                </div>
              </div>
            )}

            <div className="flex gap-2">
              <Button type="submit" disabled={createTask.isPending}>
                {createTask.isPending ? 'Creating...' : 'Create Task'}
              </Button>
              <Button type="button" variant="outline" onClick={() => navigate({ to: '/tasks' })}>
                Cancel
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
