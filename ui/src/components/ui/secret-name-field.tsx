import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useSecretNames } from '@/hooks/use-secrets'
import { isForbiddenError } from '@/lib/api-client'

const NONE = '__none__'

interface SecretNameFieldProps {
  id: string
  value: string
  onChange: (name: string) => void
  placeholder?: string
}

export function SecretNameField({ id, value, onChange, placeholder = 'Select a Secret' }: SecretNameFieldProps) {
  const { data, error, isLoading } = useSecretNames()

  if (error) {
    const hintId = `${id}-hint`
    return (
      <>
        <Input
          id={id}
          value={value}
          onChange={(event) => onChange(event.target.value)}
          placeholder="Secret name"
          aria-describedby={hintId}
          autoComplete="off"
        />
        <p id={hintId} className="text-xs text-muted-foreground">
          {isForbiddenError(error)
            ? "You can't list Secrets; type the Secret name."
            : "Secrets couldn't be listed; type the Secret name."}
        </p>
      </>
    )
  }

  return (
    <Select value={value} onValueChange={(next) => onChange(next === NONE ? '' : next)} disabled={isLoading}>
      <SelectTrigger id={id} className="max-w-full">
        <span className="min-w-0 truncate"><SelectValue placeholder={isLoading ? 'Loading Secrets...' : placeholder} /></span>
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={NONE}>None</SelectItem>
        {(data?.items ?? []).map((secret) => (
          <SelectItem key={secret.name} value={secret.name}>{secret.name}</SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}
