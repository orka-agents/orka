import { createFileRoute } from '@tanstack/react-router'
import { SkillForm } from '@/components/skills/skill-form'
import { Skeleton } from '@/components/ui/skeleton'
import { useSkill } from '@/hooks/use-skills'

export const Route = createFileRoute('/skills/$skillName_/edit')({
  component: SkillEditPage,
})

function SkillEditPage() {
  const { skillName } = Route.useParams()
  const { data: skill, isLoading } = useSkill(skillName)
  if (isLoading || !skill) return <Skeleton className="h-64 w-full" />
  return <SkillForm initial={skill} />
}
