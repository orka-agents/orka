import { describe, expect, it } from 'vitest'
import { parse } from 'yaml'
import { toYaml } from './manifests'
import { TOUR_STEPS } from './steps'

describe('toYaml', () => {
  it('writes each tour manifest as YAML that reads back as the same manifest', () => {
    const manifests = TOUR_STEPS.flatMap((step) => step.manifest ?? [])
    expect(manifests.length).toBeGreaterThan(0)
    for (const manifest of manifests) expect(parse(toYaml(manifest))).toEqual(manifest)
  })

  it('folds long values to fit the tour panel', () => {
    const resource = { spec: { ai: { prompt: 'p99 latency on checkout-service jumped from 180ms to 1.4s at 14:02 UTC. Correlate with recent deploys.' } } }
    const text = toYaml(resource)
    expect(Math.max(...text.split('\n').map((line) => line.length))).toBeLessThanOrEqual(40)
    expect(parse(text)).toEqual(resource)
  })
})
