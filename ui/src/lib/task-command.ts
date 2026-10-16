// Characters a POSIX shell reads literally outside quotes.
const plainWord = /^[A-Za-z0-9_@%+=:,./-]+$/

/**
 * Show a container Task's argv as a command line a POSIX shell (and
 * splitShellWords, which the create form uses) splits back into the same
 * words. A word that is empty or holds spaces, quotes, or shell
 * metacharacters is single-quoted, and each single quote inside it becomes
 * `'\''`, so `["sh", "-c", "echo hi; exit 1"]` reads
 * `sh -c 'echo hi; exit 1'` instead of the ambiguous `sh -c echo hi; exit 1`.
 */
export function formatShellCommand(words: string[]): string {
  return words
    .map((word) => (plainWord.test(word) ? word : `'${word.replace(/'/g, `'\\''`)}'`))
    .join(' ')
}
