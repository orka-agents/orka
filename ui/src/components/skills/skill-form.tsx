import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { PageHeader } from '@/components/layout/page-header'
import { useCreateSkill, useUpdateSkill } from '@/hooks/use-skills'
import { useUIStore } from '@/stores/ui'
import type { Skill, SkillSpec } from '@/schemas/skill'

interface SkillFormProps {
  /** When set, the form edits this skill instead of creating a new one. */
  initial?: Skill
}

export function SkillForm({ initial }: SkillFormProps) {
  const navigate = useNavigate()
  const namespace = useUIStore((s) => s.namespace)
  const createSkill = useCreateSkill()
  const updateSkill = useUpdateSkill()

  const editing = Boolean(initial)
  const [name, setName] = useState(initial?.metadata.name ?? '')
  const [displayName, setDisplayName] = useState(initial?.spec.displayName ?? '')
  const [description, setDescription] = useState(initial?.spec.description ?? '')
  const [version, setVersion] = useState(initial?.spec.version ?? '')
  const [author, setAuthor] = useState(initial?.spec.author ?? '')
  const [tags, setTags] = useState((initial?.spec.tags ?? []).join(', '))
  const [content, setContent] = useState(initial?.spec.content.inline ?? '')

  const pending = createSkill.isPending || updateSkill.isPending

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!name.trim()) {
      toast.error('Name is required')
      return
    }
    if (!description.trim()) {
      toast.error('Description is required')
      return
    }
    if (!content.trim()) {
      toast.error('Skill content is required')
      return
    }

    const parsedTags = tags.split(',').map((t) => t.trim()).filter(Boolean)
    const spec: SkillSpec = {
      description: description.trim(),
      content: { ...(initial?.spec.content ?? {}), inline: content },
      ...(displayName.trim() ? { displayName: displayName.trim() } : {}),
      ...(version.trim() ? { version: version.trim() } : {}),
      ...(author.trim() ? { author: author.trim() } : {}),
      ...(parsedTags.length ? { tags: parsedTags } : {}),
      ...(initial?.spec.source ? { source: initial.spec.source } : {}),
    }

    try {
      if (editing && initial) {
        await updateSkill.mutateAsync({ name: initial.metadata.name, spec })
        toast.success(`Skill ${initial.metadata.name} updated`)
        navigate({ to: '/skills/$skillName', params: { skillName: initial.metadata.name } })
      } else {
        await createSkill.mutateAsync({ name: name.trim(), namespace, spec })
        toast.success(`Skill ${name.trim()} created`)
        navigate({ to: '/skills' })
      }
    } catch (error) {
      toast.error(`Failed to ${editing ? 'update' : 'create'} skill: ${error instanceof Error ? error.message : 'unknown error'}`)
    }
  }

  return (
    <div className="mx-auto max-w-3xl space-y-4">
      <PageHeader
        eyebrow="Skills"
        title={editing ? `Edit ${initial?.metadata.name}` : 'New skill'}
        description="SKILL.md content is injected into the system prompt of agents that reference this skill."
      />
      <form onSubmit={handleSubmit}>
        <Card>
          <CardHeader>
            <CardTitle>Skill</CardTitle>
            <CardDescription>Follows the Agent Skills standard: metadata plus an inline SKILL.md.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              {!editing && (
                <div className="space-y-2">
                  <label htmlFor="skill-name" className="text-sm font-medium">Name</label>
                  <Input id="skill-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="code-review" />
                </div>
              )}
              <div className="space-y-2">
                <label htmlFor="skill-display-name" className="text-sm font-medium">Display name (optional)</label>
                <Input id="skill-display-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
              </div>
              <div className="space-y-2">
                <label htmlFor="skill-version" className="text-sm font-medium">Version (optional)</label>
                <Input id="skill-version" value={version} onChange={(e) => setVersion(e.target.value)} placeholder="1.0.0" />
              </div>
              <div className="space-y-2">
                <label htmlFor="skill-author" className="text-sm font-medium">Author (optional)</label>
                <Input id="skill-author" value={author} onChange={(e) => setAuthor(e.target.value)} />
              </div>
            </div>
            <div className="space-y-2">
              <label htmlFor="skill-description" className="text-sm font-medium">Description</label>
              <Input
                id="skill-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="Shown to users and models when choosing skills"
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="skill-tags" className="text-sm font-medium">Tags (comma-separated, optional)</label>
              <Input id="skill-tags" value={tags} onChange={(e) => setTags(e.target.value)} placeholder="review, golang" />
            </div>
            <div className="space-y-2">
              <label htmlFor="skill-content" className="text-sm font-medium">SKILL.md content</label>
              <textarea
                id="skill-content"
                value={content}
                onChange={(e) => setContent(e.target.value)}
                rows={16}
                spellCheck={false}
                className="w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring"
                placeholder={'# My skill\n\nInstructions the agent follows when this skill is attached…'}
              />
            </div>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="outline" onClick={() => navigate({ to: '/skills' })}>Cancel</Button>
              <Button type="submit" disabled={pending}>
                {pending ? 'Saving…' : editing ? 'Save changes' : 'Create skill'}
              </Button>
            </div>
          </CardContent>
        </Card>
      </form>
    </div>
  )
}
