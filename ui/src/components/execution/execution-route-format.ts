export function abbreviateEvidence(value?: string) {
  if (!value) return undefined
  if (value.startsWith('sha256:') && value.length > 32) {
    const digest = value.slice('sha256:'.length)
    return `sha256:${digest.slice(0, 10)}…${digest.slice(-8)}`
  }
  if (value.length > 24) return `${value.slice(0, 12)}…${value.slice(-8)}`
  return value
}

export function words(value: string) {
  return value.replace(/-/g, ' ')
}
