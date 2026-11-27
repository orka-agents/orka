export function actionErrorMessage(action: string, err: unknown): string {
  const message = err instanceof Error && err.message ? err.message.trim() : 'Unknown error'
  const prefix = `${action.toLowerCase()}:`
  const detail = message.toLowerCase().startsWith(prefix) ? message.slice(prefix.length).trim() : message
  return `${action}: ${detail || 'Unknown error'}`
}
