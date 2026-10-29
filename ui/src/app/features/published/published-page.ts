import { DatePipe } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { AdminApiService } from '../../core/admin-api.service';
import { PublishedList, PublishedRepo } from '../../core/admin.types';
import { FeedbackService } from '../../core/feedback.service';
import { formatBytes } from '../../core/format';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-published-page',
  standalone: true,
  imports: [DatePipe, FormsModule, PageHeadingComponent, RouterLink],
  templateUrl: './published-page.html',
})
export class PublishedPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly feedback = inject(FeedbackService);
  private readonly router = inject(Router);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly list = signal<PublishedList | null>(null);
  protected readonly endpoint = window.location.origin;
  protected draft = { repo_type: 'model', namespace: '', name: '', private: false };
  protected readonly formatBytes = formatBytes;

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      const list = await this.api.get<PublishedList>('/api/v1/published');
      this.list.set(list);
      if (!this.draft.namespace) this.draft.namespace = list.namespaces[0] ?? '';
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to load the published repositories.`),
      );
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    const name = this.draft.name.trim();
    if (this.busy() || !name) return;
    this.busy.set(true);
    this.feedback.clear();
    try {
      const created = await this.api.post<PublishedRepo>('/api/v1/published', {
        repo_type: this.draft.repo_type,
        namespace: this.draft.namespace,
        name,
        private: this.draft.private,
      });
      await this.router.navigate(['/published', created.id]);
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to create this repository.`),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected typeLabel(repo: PublishedRepo): string {
    return repo.repo_type === 'datasets' ? $localize`Dataset` : $localize`Model`;
  }
}
