import { Link, useLocation } from '@tanstack/react-router'
import { Compass, SearchX, type LucideIcon } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { ListAccessError } from '@/components/ui/list-access-error'
import { PageHeader } from '@/components/layout/page-header'

interface NotFoundViewProps {
  /** Page heading and browser-tab title, e.g. "Repository not found". */
  title: string
  /** What is missing, e.g. `No repository named "x" in default`. */
  headline: string
  hint?: string
  /** The way back, e.g. a link to the list page. */
  action: React.ReactNode
  icon?: LucideIcon
}

/** Standard "nothing here" page: a titled header over an empty-state card. */
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
  /** Page heading and browser-tab title, e.g. "Could not load repository". */
  title: string
  /** The failed request's error; a 403 reads as "Not authorized". */
  error: unknown
  /** What failed to load, e.g. `repository payments-main`. */
  resource: string
  /** The way back, e.g. a link to the list page. */
  action: React.ReactNode
}

/**
 * Standard "could not load" page for a detail view the token may not read
 * (or whose request failed): a titled header over the access error card.
 */
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

/** Shown inside the dashboard layout for any address no route matches. */
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
