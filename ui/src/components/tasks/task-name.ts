import { isKubernetesName } from '@/lib/connectors'

export function taskNameError(name: string): string | undefined {
  if (!name) return undefined
  if (name.length > 253) return 'Name must be at most 253 characters.'
  if (!isKubernetesName(name)) {
    return 'Use lowercase letters, numbers, "-", or ".", starting and ending with a letter or number.'
  }
  return undefined
}
