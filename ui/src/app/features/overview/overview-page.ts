import { CommonModule } from '@angular/common';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { AdminApiService } from '../../core/admin-api.service';
import { ActivityPoll, PollState } from '../../core/activity-poll';
import { AuthService } from '../../core/auth.service';
import { FeedbackService } from '../../core/feedback.service';
import { ActiveDownload, PreloadJob, Repository, Stats } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-overview-page',
  standalone: true,
  imports: [CommonModule, IconComponent, PageHeadingComponent, RouterLink],
  templateUrl: './overview-page.html',
})
export class OverviewPageComponent implements OnInit, OnDestroy {
  private readonly api = inject(AdminApiService);
  protected readonly auth = inject(AuthService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly stats = signal<Stats | null>(null);
  protected readonly repositories = signal<Repository[]>([]);
  protected readonly activeDownloads = signal<ActiveDownload[]>([]);
  protected readonly preloadJobs = signal<PreloadJob[]>([]);
  // Quick while a transfer runs, every 15 s otherwise to notice one that starts.
  private readonly transferPoll = new ActivityPoll(() => this.loadTransfers(), {
    busyMs: 2000,
    idleMs: 15000,
  });

  ngOnInit(): void {
    void this.load();
    this.transferPoll.start();
  }

  ngOnDestroy(): void {
    this.transferPoll.stop();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      const [stats, repositories] = await Promise.all([
        this.api.get<Stats>('/api/v1/stats'),
        this.api.listAll<Repository>('/api/v1/repos'),
      ]);
      this.stats.set(stats);
      this.repositories.set(repositories);
      this.feedback.clear();
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to load your overview.`),
      );
    } finally {
      this.loading.set(false);
    }
  }

  protected get hitPercent(): number {
    return Math.round((this.stats()?.hit_ratio ?? 0) * 100);
  }

  protected get hitCount(): string {
    return this.compact(this.stats()?.cache_hits ?? 0);
  }

  protected get missCount(): string {
    return this.compact(this.stats()?.cache_misses ?? 0);
  }

  protected get canManageRepositories(): boolean {
    return this.auth.principal()?.role === 'admin';
  }

  protected typeLabel(type: string): string {
    if (type === 'datasets') return $localize`Dataset`;
    if (type === 'spaces') return $localize`Space`;
    return $localize`Model`;
  }

  protected policyLabel(policy: string): string {
    if (policy === 'admin_only') return $localize`Admins only`;
    if (policy === 'allowlist') return $localize`Allowlist`;
    return $localize`Public`;
  }

  protected bytes(value: number): string {
    if (value < 1024) return `${value} B`;
    const units = ['KiB', 'MiB', 'GiB', 'TiB'];
    let size = value;
    for (const unit of units) {
      size /= 1024;
      if (size < 1024 || unit === 'TiB') return `${size.toFixed(1)} ${unit}`;
    }
    return `${value} B`;
  }

  protected preloadPercent(job: PreloadJob): number {
    return job.files_total === 0 ? 0 : Math.round((job.files_processed / job.files_total) * 100);
  }

  protected preloadStatus(status: PreloadJob['status']): string {
    switch (status) {
      case 'queued':
        return $localize`Queued`;
      case 'running':
        return $localize`Running`;
      case 'completed':
        return $localize`Completed`;
      case 'completed_with_errors':
        return $localize`Completed with errors`;
      case 'failed':
        return $localize`Failed`;
      case 'interrupted':
        return $localize`Interrupted`;
      case 'incomplete':
        return $localize`Cache incomplete`;
    }
  }

  private compact(value: number): string {
    return new Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 }).format(
      value,
    );
  }

  private async loadTransfers(): Promise<PollState> {
    try {
      const result = await this.api.get<{ items: ActiveDownload[]; preloads: PreloadJob[] }>(
        '/api/v1/downloads/active',
      );
      this.activeDownloads.set(result.items);
      this.preloadJobs.set(result.preloads);
      const preloading = result.preloads.some(
        (job) => job.status === 'queued' || job.status === 'running',
      );
      return result.items.length > 0 || preloading ? 'busy' : 'idle';
    } catch {
      // Keep the most recent transfer state visible if a poll briefly fails.
      return this.auth.principal() ? 'idle' : 'stop';
    }
  }
}
