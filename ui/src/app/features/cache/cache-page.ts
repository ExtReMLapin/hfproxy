import { CommonModule } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import { CacheEntry } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-cache-page',
  standalone: true,
  imports: [CommonModule, FormsModule, IconComponent, PageHeadingComponent],
  templateUrl: './cache-page.html',
})
export class CachePageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly entries = signal<CacheEntry[]>([]);
  protected filters = { repo_id: '', path: '' };

  ngOnInit(): void {
    void this.search();
  }

  protected async search(): Promise<void> {
    this.loading.set(true);
    try {
      const query = new URLSearchParams({ limit: '100' });
      if (this.filters.repo_id.trim()) query.set('repo_id', this.filters.repo_id.trim());
      if (this.filters.path.trim()) query.set('path', this.filters.path.trim());
      this.entries.set(
        (await this.api.get<{ items: CacheEntry[] }>(`/api/v1/cache/entries?${query}`)).items,
      );
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, 'Unable to search cached files.'),
      );
    } finally {
      this.loading.set(false);
    }
  }

  protected async purge(entry: CacheEntry): Promise<void> {
    if (
      !window.confirm(
        `Remove ${entry.path} from the cache for ${entry.repo_id}? This deletes the stored artifact.`,
      )
    )
      return;
    this.busy.set(true);
    try {
      await this.api.post(`/api/v1/cache/entries/${encodeURIComponent(entry.id)}/purge`, {});
      await this.search();
      this.feedback.showSuccess('Cached artifact purged.');
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, 'Unable to purge the cached artifact.'),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected typeLabel(type: string): string {
    return type === 'datasets' ? 'Dataset' : type === 'spaces' ? 'Space' : 'Model';
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
}
