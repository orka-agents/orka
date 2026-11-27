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
        i++
      } else if (ch === '\\' && i + 1 < input.length && '"\\$`'.includes(input[i + 1])) {
        current += input[++i]
      } else {
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
      if (i + 1 >= input.length) {
        return { error: 'Trailing backslash in command' }
      }
      if (input[i + 1] === '\n') {
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
