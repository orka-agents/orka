import { isKubernetesName } from '@/lib/connectors'

// taskNameError mirrors the API server's Task name rule (a DNS-1123
// subdomain) so a bad name is explained inline instead of surfacing as the
// server's raw validation regex after the request. Empty names are left to
// each form: required on create, auto-generated on fork.
export function taskNameError(name: string): string | undefined {
  if (!name) return undefined
  if (name.length > 253) return 'Name must be at most 253 characters.'
  if (!isKubernetesName(name)) {
    return 'Use lowercase letters, numbers, "-", or ".", starting and ending with a letter or number.'
  }
  return undefined
}
