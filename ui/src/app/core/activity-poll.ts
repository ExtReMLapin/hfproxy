/** What a poll found: work in progress, nothing going on, or no need to go on. */
export type PollState = 'busy' | 'idle' | 'stop';

export interface PollIntervals {
  /** Delay between polls while something is in progress. */
  busyMs: number;
  /** Delay while nothing is in progress; null waits for poke() or the tab coming back. */
  idleMs: number | null;
}

/**
 * Polls only as often as the page needs. Quick while a transfer is running, slow
 * (or not at all) when nothing is, and not at all in a hidden tab, which catches
 * up as soon as it is shown again.
 */
export class ActivityPoll {
  private timer?: number;
  private running = false;
  private stopped = true;
  private readonly onVisible = () => {
    if (!document.hidden && !this.stopped) void this.run();
  };

  constructor(
    private readonly load: () => Promise<PollState>,
    private readonly intervals: PollIntervals,
  ) {}

  start(): void {
    this.stopped = false;
    document.addEventListener('visibilitychange', this.onVisible);
    void this.run();
  }

  stop(): void {
    this.stopped = true;
    document.removeEventListener('visibilitychange', this.onVisible);
    this.cancelTimer();
  }

  /** Polls now, for instance right after the user started something. */
  poke(): void {
    if (!this.stopped) void this.run();
  }

  private async run(): Promise<void> {
    if (this.running || document.hidden) return;
    this.running = true;
    this.cancelTimer();
    let state: PollState = 'idle';
    try {
      state = await this.load();
    } catch {
      // A failed poll keeps what is shown; try again at the slow pace.
    } finally {
      this.running = false;
    }
    if (this.stopped || state === 'stop') {
      this.stop();
      return;
    }
    const delay = state === 'busy' ? this.intervals.busyMs : this.intervals.idleMs;
    if (delay !== null) this.timer = window.setTimeout(() => void this.run(), delay);
  }

  private cancelTimer(): void {
    if (this.timer !== undefined) window.clearTimeout(this.timer);
    this.timer = undefined;
  }
}
