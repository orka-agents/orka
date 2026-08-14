import { createFileRoute } from '@tanstack/react-router'
import { SkillForm } from '@/components/skills/skill-form'

export const Route = createFileRoute('/skills/new')({
  component: SkillForm,
})
