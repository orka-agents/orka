import { Link } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { PageHeader } from '@/components/layout/page-header'
import { useSkillList } from '@/hooks/use-skills'

export function SkillList() {
  const { data, isLoading } = useSkillList()
  const items = data?.items ?? []

  return (
    <div className="space-y-4">
      <PageHeader
        title="Skills"
        description="Reusable instruction packs injected into agent prompts"
        action={
          <Button asChild>
            <Link to="/skills/new">
              <Plus className="mr-2 h-4 w-4" />
              New skill
            </Link>
          </Button>
        }
      />
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Description</TableHead>
              <TableHead>Version</TableHead>
              <TableHead>Tags</TableHead>
              <TableHead>Phase</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              Array.from({ length: 3 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 5 }).map((_, j) => (
                    <TableCell key={j}><Skeleton className="h-4 w-20" /></TableCell>
                  ))}
                </TableRow>
              ))
            ) : items.length === 0 ? (
              <TableRow>
                <TableCell colSpan={5} className="py-8 text-center text-muted-foreground">
                  No skills yet. Create one to share instructions across agents.
                </TableCell>
              </TableRow>
            ) : (
              items.map((skill) => (
                <TableRow key={skill.name}>
                  <TableCell>
                    <Link
                      to="/skills/$skillName"
                      params={{ skillName: skill.name }}
                      className="font-medium hover:underline"
                    >
                      {skill.displayName || skill.name}
                    </Link>
                    {skill.displayName && skill.displayName !== skill.name && (
                      <p className="font-mono text-xs text-muted-foreground">{skill.name}</p>
                    )}
                  </TableCell>
                  <TableCell className="max-w-md truncate">{skill.description}</TableCell>
                  <TableCell className="font-mono text-xs">{skill.version || '-'}</TableCell>
                  <TableCell>
                    <div className="flex flex-wrap gap-1">
                      {(skill.tags ?? []).slice(0, 4).map((tag) => (
                        <Badge key={tag} variant="secondary">{tag}</Badge>
                      ))}
                    </div>
                  </TableCell>
                  <TableCell>
                    {skill.phase === 'Ready' ? (
                      <Badge className="bg-status-succeeded-bg text-status-succeeded" variant="secondary">Ready</Badge>
                    ) : skill.phase ? (
                      <Badge className="bg-status-failed-bg text-status-failed" variant="secondary">{skill.phase}</Badge>
                    ) : (
                      <Badge variant="secondary">Unknown</Badge>
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
