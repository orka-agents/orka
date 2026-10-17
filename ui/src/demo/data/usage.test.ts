import { describe, expect, it } from 'vitest'
import { usageLedger } from './usage'

describe('demo usage ledger', () => {
  it('gives every issue and pull request its own number within a repository', () => {
    const numbers = [...usageLedger.works, ...usageLedger.pullRequests].map((item) => `${item.repository}#${item.number}`)
    expect(new Set(numbers).size).toBe(numbers.length)
  })
})
