import { describe, it, expect } from 'vitest'
import { everyScheduleError, isCredentialFreeGitHubRepositoryURL, isCronSchedule } from './repository-form-validation'

describe('isCredentialFreeGitHubRepositoryURL', () => {
  it.each([
    'https://github.com/org/repo',
    'https://github.com/org/repo.git',
    '  https://github.com/org/repo  ',
    'git@github.com:org/repo',
    'git@github.com:org/repo.git',
  ])('accepts %s', (url) => {
    expect(isCredentialFreeGitHubRepositoryURL(url)).toBe(true)
  })

  it.each([
    '',
    'not a url',
    'http://github.com/org/repo',
    'https://gitlab.com/org/repo',
    'https://token@github.com/org/repo',
    'https://user:pass@github.com/org/repo',
    'https://github.com/org',
    'https://github.com/org/repo/pull/1',
    'https://github.com/org/repo/tree/main',
    'https://github.com/org/repo?tab=readme',
    'https://github.com/org/repo#readme',
  ])('rejects %s', (url) => {
    expect(isCredentialFreeGitHubRepositoryURL(url)).toBe(false)
  })
})

describe('isCronSchedule', () => {
  it.each([
    '0 */6 * * *',
    '*/30 * * * *',
    '0 9 * * MON-FRI',
    ' 15 2 1,15 * ? ',
    '59 23 31 12 6',
    '0 0 1 1 0',
    '0-30/10 8-18 1-7,15 jan-JUN sun,Sat',
    '5/15 * * * *',
    '0 6 * DEC *',
    '@daily',
    '@every 1h30m',
    '@every 5m',
    '@every 1m',
    '@every 60s',
    '@every 90s',
    '@every 1.5m',
    '@every .5h',
    '@every 1m0.5s',
    '@every +10m',
    '@every 2h',
    'TZ=UTC @every 15m',
    'CRON_TZ=Europe/Berlin 0 6 * * *',
    'TZ=UTC 0 6 * * *',
  ])('accepts %s', (schedule) => {
    expect(isCronSchedule(schedule)).toBe(true)
  })

  it.each([
    '',
    'every hour',
    '0 */6 * *',
    '0 */6 * * * *',
    '@sometimes',
    '0 6 * * * ; rm',
    '60 * * * *',
    '0 24 * * *',
    '0 0 0 * *',
    '0 0 32 * *',
    '0 0 * 0 *',
    '0 0 * 13 *',
    // robfig/cron's standard parser has no weekday 7.
    '0 0 * * 7',
    '0 0 * * 1-7',
    '30-10 * * * *',
    '*/0 * * * *',
    '1/2/3 * * * *',
    '1-2-3 * * * *',
    // robfig/cron skips empty list items; an empty item is a typo here.
    '1,,2 * * * *',
    '-1 * * * *',
    '0 MON * * *',
    '0 0 * * JAN',
    '0 0 * * monday',
    // @every needs a Go duration of at least one minute.
    '@every 0',
    '@every 0s',
    '@every -0',
    '@every -5m',
    '@every 1s',
    '@every 30s',
    '@every 59s',
    '@every 59.9s',
    '@every 999ms',
    '@every 1000000us',
    '@every',
    '@every ',
    '@every 5',
    '@every 5 minutes',
    '@every 5min',
    '@every 1d',
    '@every m',
    '@every .m',
    '@every 5m-1s',
    '@every +-5m',
    '@every 5M',
    // robfig/cron splits "@every " off, so a second space is part of the duration.
    '@every  5m',
    '@every\t5m',
    // Past int64 nanoseconds, which time.ParseDuration refuses.
    '@every 3000000h',
    'TZ=UTC @every 30s',
  ])('rejects %s', (schedule) => {
    expect(isCronSchedule(schedule)).toBe(false)
  })
})

describe('everyScheduleError', () => {
  it.each(['@every 0s', '@every -5m', '@every 30s', 'TZ=UTC @every 1s'])('asks for at least a minute for %s', (schedule) => {
    expect(everyScheduleError(schedule)).toBe(`Schedule "${schedule}" needs an interval of at least 1m, such as @every 5m or @every 1h30m.`)
  })

  it.each(['@every', '@every 5', '@every five minutes', '@every 1d'])('asks for a Go duration for %s', (schedule) => {
    expect(everyScheduleError(schedule)).toBe(`Schedule "${schedule}" needs a Go duration after @every, such as @every 5m or @every 1h30m.`)
  })

  it.each(['@every 5m', '@every 1h30m', '0 */6 * * *', '@daily', 'every 5m'])('has nothing to say about %s', (schedule) => {
    expect(everyScheduleError(schedule)).toBeUndefined()
  })
})
