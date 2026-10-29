import { ActivityPoll, PollState } from './activity-poll';

describe('ActivityPoll', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  function poll(states: PollState[], idleMs: number | null = null) {
    const load = vi.fn(async () => states.shift() ?? 'idle');
    return { load, poll: new ActivityPoll(load, { busyMs: 2000, idleMs }) };
  }

  it('polls quickly while busy and stops asking once nothing is going on', async () => {
    const { load, poll: p } = poll(['busy', 'busy', 'idle']);
    p.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(load).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2000);
    await vi.advanceTimersByTimeAsync(2000);
    expect(load).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(load).toHaveBeenCalledTimes(3);
    p.stop();
  });

  it('keeps a slow pace when asked to', async () => {
    const { load, poll: p } = poll([], 15_000);
    p.start();
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(45_000);
    expect(load).toHaveBeenCalledTimes(4);
    p.stop();
  });

  it('polls at once when poked, then follows what it finds', async () => {
    const { load, poll: p } = poll(['idle', 'busy', 'idle']);
    p.start();
    await vi.advanceTimersByTimeAsync(0);
    p.poke();
    await vi.advanceTimersByTimeAsync(0);
    expect(load).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(2000);
    expect(load).toHaveBeenCalledTimes(3);
    p.stop();
  });

  it('does not poll after being stopped, or when told to stop', async () => {
    const { load, poll: p } = poll(['stop']);
    p.start();
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(load).toHaveBeenCalledTimes(1);
    p.poke();
    await vi.advanceTimersByTimeAsync(0);
    expect(load).toHaveBeenCalledTimes(1);
  });
});
