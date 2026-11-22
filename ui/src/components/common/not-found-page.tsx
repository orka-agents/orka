import { Link, useLocation } from '@tanstack/react-router'
import { Compass, SearchX, type LucideIcon } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'
import { PageHeader } from '@/components/layout/page-header'

interface NotFoundViewProps {
  title: string
  headline: string
  hint?: string
  action: React.ReactNode
  icon?: LucideIcon
}

export function NotFoundView({ title, headline, hint, action, icon = SearchX }: NotFoundViewProps) {
  return (
    <div className="space-y-4">
      <PageHeader title={title} />
      <Card>
        <CardContent>
          <EmptyState icon={icon} headline={headline} hint={hint} action={action} />
        </CardContent>
      </Card>
    </div>
  )
}

interface AccessErrorViewProps {
  title: string
  error: unknown
  resource: string
  action: React.ReactNode
}

export function AccessErrorView({ title, error, resource, action }: AccessErrorViewProps) {
  return (
    <div className="space-y-4">
      <PageHeader title={title} />
      <Card>
        <CardContent>
          <ListAccessError error={error} resource={resource} action={action} />
        </CardContent>
      </Card>
    </div>
  )
}

export function NotFoundPage() {
  const { pathname } = useLocation()
  return (
    <NotFoundView
      title="Page not found"
      icon={Compass}
      headline={`Nothing lives at ${pathname}`}
      hint="The address may be mistyped, or the page may have moved."
      action={
        <Button asChild>
          <Link to="/">Go to Dashboard</Link>
        </Button>
      }
    />
  )
}
