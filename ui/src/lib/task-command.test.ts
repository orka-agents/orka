import { describe, it, expect } from 'vitest'
import { formatShellCommand } from './task-command'
import { splitShellWords } from './shell-words'

describe('formatShellCommand', () => {
  it('leaves plain words unquoted', () => {
    expect(formatShellCommand(['echo', 'hello', '--flag=a/b:c', '-n'])).toBe('echo hello --flag=a/b:c -n')
  })

  it('single-quotes words with spaces or shell metacharacters', () => {
    expect(formatShellCommand(['sh', '-c', 'echo attempt; exit 1'])).toBe(`sh -c 'echo attempt; exit 1'`)
    expect(formatShellCommand(['echo', '$HOME', 'a|b', '*'])).toBe(`echo '$HOME' 'a|b' '*'`)
  })

  it('quotes empty words and escapes embedded quotes', () => {
    expect(formatShellCommand(['printf', ''])).toBe(`printf ''`)
    expect(formatShellCommand(['echo', `it's "here"`])).toBe(`echo 'it'\\''s "here"'`)
  })

  it('round-trips through the create form splitter', () => {
    const argv = ['sh', '-c', `echo "it's" \\ done; exit 1`, '', 'tab\there', 'line\nbreak']
    expect(splitShellWords(formatShellCommand(argv))).toEqual({ words: argv })
  })
})
