/**
 * Split a command line into words the way a POSIX shell would, without
 * expansions: whitespace separates words, single quotes preserve everything
 * literally, double quotes preserve everything except backslash escapes of
 * `"` `\` and `$`, and an unquoted backslash escapes the next character.
 *
 * Returns `{ words }` on success or `{ error }` when a quote or a trailing
 * backslash escape is left open, so
 * `sh -c "echo hi"` becomes `["sh", "-c", "echo hi"]` instead of the naive
 * whitespace split that leaves the quote characters inside the words.
 */
export function splitShellWords(input: string): { words: string[] } | { error: string } {
  const words: string[] = []
  let current = ''
  let inWord = false
  let quote: '"' | "'" | null = null

  for (let i = 0; i < input.length; i++) {
    const ch = input[i]

    if (quote === "'") {
      if (ch === "'") {
        quote = null
      } else {
        current += ch
      }
      continue
    }

    if (quote === '"') {
      if (ch === '"') {
        quote = null
      } else if (ch === '\\' && i + 1 < input.length && input[i + 1] === '\n') {
        // Backslash-newline is a line continuation inside double quotes too.
        i++
      } else if (ch === '\\' && i + 1 < input.length && '"\\$`'.includes(input[i + 1])) {
        current += input[++i]
      } else {
        // Every other backslash is literal inside double quotes.
        current += ch
      }
      continue
    }

    if (ch === '"' || ch === "'") {
      quote = ch
      inWord = true
      continue
    }
    if (ch === '\\') {
      // A backslash with nothing after it is an unterminated escape, not a
      // no-op: a shell would wait for more input, so reject it instead of
      // silently dropping it.
      if (i + 1 >= input.length) {
        return { error: 'Trailing backslash in command' }
      }
      if (input[i + 1] === '\n') {
        // Backslash-newline is a line continuation: it joins the lines and
        // contributes no character to the word.
        i++
        continue
      }
      current += input[++i]
      inWord = true
      continue
    }
    if (/\s/.test(ch)) {
      if (inWord) {
        words.push(current)
        current = ''
        inWord = false
      }
      continue
    }
    current += ch
    inWord = true
  }

  if (quote) {
    return { error: `Unterminated ${quote === '"' ? 'double' : 'single'} quote in command` }
  }
  if (inWord) words.push(current)
  return { words }
}
