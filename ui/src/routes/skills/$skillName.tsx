import { createFileRoute } from '@tanstack/react-router'
import { SkillDetail } from '@/components/skills/skill-detail'

export const Route = createFileRoute('/skills/$skillName')({
  component: SkillDetailPage,
})

function SkillDetailPage() {
  const { skillName } = Route.useParams()
  return <SkillDetail skillName={skillName} />
}
