// actionErrorMessage formats a failed task action for a toast or inline
// error. Server errors often already start with the same "failed to <action>:"
// prefix the UI adds, so a repeated prefix is dropped instead of reading
// "Failed to create task: failed to create task: ...".
export function actionErrorMessage(action: string, err: unknown): string {
  const message = err instanceof Error && err.message ? err.message.trim() : 'Unknown error'
  const prefix = `${action.toLowerCase()}:`
  const detail = message.toLowerCase().startsWith(prefix) ? message.slice(prefix.length).trim() : message
  return `${action}: ${detail || 'Unknown error'}`
}
