import { describe, expect, it } from 'vitest'
import { toYaml } from './manifests'

describe('toYaml', () => {
  it('writes maps, lists and scalars, leaving out absent and empty values', () => {
    expect(toYaml({
      kind: 'Agent',
      spec: {
        repoURL: 'https://github.com/northwind-labs/checkout-service',
        schedule: '*/15 * * * *',
        timeout: '15m',
        mode: 'true',
        retries: 2,
        enabled: false,
        tools: [{ name: 'web_search' }, { name: 'slack_post', enabled: true }],
        labels: ['bug', 'orka:implement'],
        unset: undefined,
        empty: {},
      },
    })).toBe([
      'kind: Agent',
      'spec:',
      '  repoURL: https://github.com/northwind-labs/checkout-service',
      '  schedule: "*/15 * * * *"',
      '  timeout: "15m"',
      '  mode: "true"',
      '  retries: 2',
      '  enabled: false',
      '  tools:',
      '    - name: web_search',
      '    - name: slack_post',
      '      enabled: true',
      '  labels:',
      '    - bug',
      '    - orka:implement',
    ].join('\n'))
  })

  it('folds long prose to fit the tour panel', () => {
    const prompt = 'p99 latency on checkout-service jumped from 180ms to 1.4s at 14:02 UTC. Correlate with recent deploys.'
    const yaml = toYaml({ ai: { prompt } })
    expect(yaml.split('\n').slice(0, 2)).toEqual(['ai:', '  prompt: >-'])
    expect(yaml.split('\n').slice(2).every((line) => line.startsWith('    ') && line.length <= 4 + 44)).toBe(true)
    // A folded block reads back as the original text, its line breaks becoming spaces.
    expect(yaml.split('\n').slice(2).map((line) => line.trim()).join(' ')).toBe(prompt)
  })

  it('quotes long text that folding would change', () => {
    const prompt = 'Repeat exactly "a  b" and explain why the spacing matters to the reader.'
    expect(toYaml({ prompt })).toBe(`prompt: ${JSON.stringify(prompt)}`)
  })
})
