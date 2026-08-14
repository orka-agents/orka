import { useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { Pencil, Trash2 } from 'lucide-react'
import { toast } from 'sonner'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { useDeleteSkill, useSkill, useSkillContent } from '@/hooks/use-skills'

export function SkillDetail({ skillName }: { skillName: string }) {
  const navigate = useNavigate()
  const { data: skill, isLoading, isError } = useSkill(skillName)
  const { data: content } = useSkillContent(skillName)
  const deleteSkill = useDeleteSkill()
  const [confirming, setConfirming] = useState(false)

  const handleDelete = async () => {
    if (!confirming) {
      setConfirming(true)
      return
    }
    try {
      await deleteSkill.mutateAsync(skillName)
      toast.success(`Skill ${skillName} deleted`)
      navigate({ to: '/skills' })
    } catch (error) {
      toast.error(`Failed to delete skill: ${error instanceof Error ? error.message : 'unknown error'}`)
      setConfirming(false)
    }
  }

  if (isLoading) {
    return <Skeleton className="h-64 w-full" />
  }
  if (isError || !skill) {
    return <PageHeader eyebrow="Skills" title={skillName} description="Skill not found in this namespace." />
  }

  const files = Object.keys(skill.spec.content.files ?? {})

  return (
    <div className="space-y-4">
      <PageHeader
        eyebrow="Skills"
        title={skill.spec.displayName || skill.metadata.name}
        description={skill.spec.description}
        action={
          <>
            <Button asChild variant="outline">
              <Link to="/skills/$skillName/edit" params={{ skillName }}>
                <Pencil className="mr-2 h-4 w-4" />
                Edit
              </Link>
            </Button>
            <Button variant={confirming ? 'destructive' : 'outline'} onClick={handleDelete} disabled={deleteSkill.isPending}>
              <Trash2 className="mr-2 h-4 w-4" />
              {confirming ? 'Confirm delete' : 'Delete'}
            </Button>
          </>
        }
      />
      <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
        {skill.spec.version && <Badge variant="secondary">v{skill.spec.version}</Badge>}
        {skill.spec.author && <span>by {skill.spec.author}</span>}
        {(skill.spec.tags ?? []).map((tag) => (
          <Badge key={tag} variant="secondary">{tag}</Badge>
        ))}
        {skill.status?.phase && (
          <Badge
            variant="secondary"
            className={skill.status.phase === 'Ready'
              ? 'bg-status-succeeded-bg text-status-succeeded'
              : 'bg-status-failed-bg text-status-failed'}
          >
            {skill.status.phase}
          </Badge>
        )}
      </div>
      <Card>
        <CardHeader>
          <CardTitle>SKILL.md</CardTitle>
        </CardHeader>
        <CardContent>
          <ScrollArea className="max-h-[32rem]">
            <pre className="whitespace-pre-wrap break-words font-mono text-xs leading-5">
              {content ?? skill.spec.content.inline}
            </pre>
          </ScrollArea>
        </CardContent>
      </Card>
      {files.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>Bundled files</CardTitle>
          </CardHeader>
          <CardContent className="space-y-1">
            {files.map((path) => (
              <p key={path} className="font-mono text-xs">{path}</p>
            ))}
          </CardContent>
        </Card>
      )}
    </div>
  )
}
