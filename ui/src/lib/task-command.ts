const plainWord = /^[A-Za-z0-9_@%+=:,./-]+$/

export function formatShellCommand(words: string[]): string {
  return words
    .map((word) => (plainWord.test(word) ? word : `'${word.replace(/'/g, `'\\''`)}'`))
    .join(' ')
}
