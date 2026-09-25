import { describe, it, expect } from 'vitest'
import { splitShellWords } from './shell-words'

describe('splitShellWords', () => {
  it('splits on whitespace', () => {
    expect(splitShellWords('ls -la  /tmp')).toEqual({ words: ['ls', '-la', '/tmp'] })
  })

  it('returns no words for an empty or blank string', () => {
    expect(splitShellWords('')).toEqual({ words: [] })
    expect(splitShellWords('   ')).toEqual({ words: [] })
  })

  it('keeps double-quoted text as one word without the quotes', () => {
    expect(splitShellWords('sh -c "echo UI_TASK_OK"')).toEqual({ words: ['sh', '-c', 'echo UI_TASK_OK'] })
  })

  it('keeps single-quoted text literally', () => {
    expect(splitShellWords(`sh -c 'echo "$HOME" \\n'`)).toEqual({ words: ['sh', '-c', 'echo "$HOME" \\n'] })
  })

  it('preserves empty quoted words and joins adjacent quoted segments', () => {
    expect(splitShellWords(`printf "" ''`)).toEqual({ words: ['printf', '', ''] })
    expect(splitShellWords(`echo foo"bar baz"'qux'`)).toEqual({ words: ['echo', 'foobar bazqux'] })
  })

  it('honors backslash escapes outside quotes', () => {
    expect(splitShellWords('echo hello\\ world \\"quoted\\"')).toEqual({ words: ['echo', 'hello world', '"quoted"'] })
  })

  it('honors backslash escapes of " \\ $ inside double quotes only', () => {
    expect(splitShellWords('echo "a \\"b\\" \\$c \\\\ \\n"')).toEqual({ words: ['echo', 'a "b" $c \\ \\n'] })
  })

  it('keeps a backslash before an ordinary character inside double quotes', () => {
    expect(splitShellWords('printf "a\\q" "x\\n"')).toEqual({ words: ['printf', 'a\\q', 'x\\n'] })
  })

  it('treats backslash-newline as a line continuation outside and inside double quotes', () => {
    expect(splitShellWords('foo\\\nbar baz')).toEqual({ words: ['foobar', 'baz'] })
    expect(splitShellWords('"foo\\\nbar"')).toEqual({ words: ['foobar'] })
    // A single-quoted backslash-newline is literal.
    expect(splitShellWords("'foo\\\nbar'")).toEqual({ words: ['foo\\\nbar'] })
  })

  it('rejects a trailing unquoted backslash instead of dropping it', () => {
    expect(splitShellWords('echo foo\\')).toEqual({ error: 'Trailing backslash in command' })
    expect(splitShellWords('\\')).toEqual({ error: 'Trailing backslash in command' })
    // Inside quotes a trailing backslash is literal; the open quote is the error.
    expect(splitShellWords('echo "foo\\')).toEqual({ error: 'Unterminated double quote in command' })
  })

  it('rejects unterminated double and single quotes', () => {
    expect(splitShellWords('sh -c "echo hi')).toEqual({ error: 'Unterminated double quote in command' })
    expect(splitShellWords("sh -c 'echo hi")).toEqual({ error: 'Unterminated single quote in command' })
  })
})
