import { describe, expect, it } from 'vitest'
import { render, screen } from '@/test/test-utils'
import { PostureLineView } from './posture-line'
import { makePosture } from '@/test/fixtures/oversight'

describe('PostureLineView latency', () => {
  it('adds "decided within ~p95" only when latency is reported', () => {
    const { unmount } = render(<PostureLineView posture={makePosture({ basis: { ...makePosture().basis, decisionLatency: { count: 5, p50Seconds: 2, p95Seconds: 41, maxSeconds: 60 } } })} incidents={[]} />)
    expect(screen.getByTestId('posture-latency')).toHaveTextContent('decided within ~41s')
    unmount()
    render(<PostureLineView posture={makePosture()} incidents={[]} />)
    expect(screen.queryByTestId('posture-latency')).not.toBeInTheDocument()
  })

  it('says why there is no figure when every record was replayed', () => {
    const { unmount } = render(<PostureLineView posture={makePosture({ basis: { ...makePosture().basis,
      decisionLatency: { count: 0, replayedCount: 20, p50Seconds: 0, p95Seconds: 0, maxSeconds: 0 } } })} incidents={[]} />)
    expect(screen.getByTestId('posture-latency')).toHaveTextContent('decision latency: n/a (replayed)')
    unmount()
    render(<PostureLineView posture={makePosture({ basis: { ...makePosture().basis,
      decisionLatency: { count: 3, replayedCount: 20, p50Seconds: 2, p95Seconds: 3, maxSeconds: 3 } } })} incidents={[]} />)
    expect(screen.getByTestId('posture-latency')).toHaveTextContent('decided within ~3s')
    expect(screen.getByTestId('posture-latency')).toHaveAttribute('title', 'Decision latency: p50 2s · p95 3s · max 3s (n=3 live; 20 replayed left out)')
  })
})
