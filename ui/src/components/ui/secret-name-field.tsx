import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useSecretNames } from '@/hooks/use-secrets'
import { isForbiddenError } from '@/lib/api-client'

// Radix Select items cannot carry an empty value, so "None" uses a sentinel
// that maps back to the empty name.
const NONE = '__none__'

interface SecretNameFieldProps {
  /** Id of the control, so the caller's `<label htmlFor>` names it. */
  id: string
  value: string
  onChange: (name: string) => void
  /** Placeholder for the Secret picker. */
  placeholder?: string
}

/**
 * Picks a Secret in the current namespace by name. Listing Secrets is its own
 * permission (orka-api-editor-role deliberately lacks it), so when the list is
 * forbidden or fails the field turns into a text input instead of leaving an
 * empty dropdown with no way to name the Secret.
 */
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
