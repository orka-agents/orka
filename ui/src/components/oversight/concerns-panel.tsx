import { MessageCircleQuestion } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useOversightConfig } from '@/hooks/use-oversight'

/** The questions the monitor asks, by the names an operator gave them. */
export function ConcernsPanel() {
  const { data: config } = useOversightConfig()
  const concerns = config?.concerns ?? []
  return (
    <Card role="region" aria-label="What the monitor asks">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <MessageCircleQuestion className="size-4" aria-hidden="true" /> What the monitor asks
        </CardTitle>
      </CardHeader>
      <CardContent>
        {concerns.length === 0 ? (
          <p className="text-sm text-muted-foreground">No concerns declared.</p>
        ) : (
          <ul className="space-y-2">
            {concerns.map((concern) => (
              <li key={concern.id} className="text-sm">
                <span className="font-medium">{concern.name}</span>
                {concern.meaning && <span className="text-muted-foreground"> — {concern.meaning}</span>}
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  )
}
