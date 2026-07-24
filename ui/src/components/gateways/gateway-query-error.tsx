import { Card, CardContent } from '@/components/ui/card'

export function GatewayQueryError({ label, error }: { label: string; error: unknown }) {
  const message = error instanceof Error ? error.message : 'Unknown error'
  return (
    <Card className="border-destructive/30 bg-destructive/5">
      <CardContent className="py-8 text-center">
        <p className="font-medium text-destructive">Could not load {label}.</p>
        <p className="mt-2 text-sm text-muted-foreground">{message}</p>
      </CardContent>
    </Card>
  )
}
