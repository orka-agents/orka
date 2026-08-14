import { createFileRoute } from '@tanstack/react-router'
import { SkillList } from '@/components/skills/skill-list'

export const Route = createFileRoute('/skills/')({
  component: SkillList,
})
