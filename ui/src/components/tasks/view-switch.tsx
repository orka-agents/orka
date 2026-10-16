import { Button } from '@/components/ui/button'

interface ViewSwitchProps<T extends string> {
  label: string
  value: T
  options: readonly { value: T; label: string }[]
  onChange: (value: T) => void
}

/** Two or three ways to look at the same thing, as pressed toggle buttons. */
export function ViewSwitch<T extends string>({ label, value, options, onChange }: ViewSwitchProps<T>) {
  return (
    <div role="group" aria-label={label} className="inline-flex gap-1 rounded-md border p-0.5">
      {options.map((option) => (
        <Button
          key={option.value}
          size="sm"
          variant={value === option.value ? 'secondary' : 'ghost'}
          aria-pressed={value === option.value}
          onClick={() => onChange(option.value)}
        >
          {option.label}
        </Button>
      ))}
    </div>
  )
}
