import { DatePipe } from '@angular/common';
import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { AdminApiService } from '../../core/admin-api.service';
import { PublishedCommit, PublishedDetail, PublishedEntry } from '../../core/admin.types';
import { FeedbackService } from '../../core/feedback.service';
import { formatBytes } from '../../core/format';
import { MarkdownComponent } from '../../shared/markdown/markdown';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

type Tab = 'files' | 'card' | 'history' | 'use';

@Component({
  selector: 'app-published-detail-page',
  standalone: true,
  imports: [DatePipe, FormsModule, MarkdownComponent, PageHeadingComponent, RouterLink],
  templateUrl: './published-detail-page.html',
})
export class PublishedDetailPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly feedback = inject(FeedbackService);
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);
  protected readonly formatBytes = formatBytes;
  protected readonly endpoint = window.location.origin;

  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly detail = signal<PublishedDetail | null>(null);
  protected readonly entries = signal<PublishedEntry[]>([]);
  protected readonly truncated = signal(false);
  protected readonly commits = signal<PublishedCommit[]>([]);
  protected readonly folder = signal('');
  protected readonly revision = signal('main');
  protected readonly tab = signal<Tab>('files');
  protected readonly selected = signal<Set<string>>(new Set());
  protected readonly confirming = signal('');
  protected readonly progress = signal<{ sent: number; total: number } | null>(null);
  protected message = '';
  private id = '';

  protected readonly crumbs = computed(() => {
    const parts = this.folder() ? this.folder().split('/') : [];
    return parts.map((name, index) => ({ name, path: parts.slice(0, index + 1).join('/') }));
  });

  /** Branches can be committed to; a tag or a commit is read-only. */
  protected readonly onBranch = computed(
    () => this.detail()?.branches.includes(this.revision()) ?? false,
  );

  ngOnInit(): void {
    this.id = this.route.snapshot.paramMap.get('id') ?? '';
    void this.load();
  }

  private url(suffix = ''): string {
    return `/api/v1/published/${encodeURIComponent(this.id)}${suffix}`;
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      const query = `revision=${encodeURIComponent(this.revision())}`;
      const detail = await this.api.get<PublishedDetail>(`${this.url()}?${query}`);
      this.detail.set(detail);
      await this.loadTree();
      if (this.tab() === 'history') await this.loadCommits();
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to load this repository.`),
      );
    } finally {
      this.loading.set(false);
    }
  }

  private async loadTree(): Promise<void> {
    const query = `revision=${encodeURIComponent(this.revision())}&path=${encodeURIComponent(this.folder())}`;
    const tree = await this.api.get<{ items: PublishedEntry[]; truncated: boolean }>(
      `${this.url('/tree')}?${query}`,
    );
    this.entries.set(tree.items);
    this.truncated.set(tree.truncated);
    this.selected.set(new Set());
  }

  private async loadCommits(): Promise<void> {
    const result = await this.api.get<{ items: PublishedCommit[] }>(
      `${this.url('/commits')}?revision=${encodeURIComponent(this.revision())}`,
    );
    this.commits.set(result.items);
  }

  protected async show(tab: Tab): Promise<void> {
    this.tab.set(tab);
    if (tab === 'history') {
      try {
        await this.loadCommits();
      } catch (error) {
        this.feedback.showError(
          AdminApiService.errorMessage(error, $localize`Unable to load the history.`),
        );
      }
    }
  }

  protected async chooseRevision(revision: string): Promise<void> {
    this.revision.set(revision);
    this.folder.set('');
    await this.load();
  }

  protected async open(path: string): Promise<void> {
    this.folder.set(path);
    try {
      await this.loadTree();
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to open this folder.`),
      );
    }
  }

  protected download(entry: PublishedEntry): string {
    const detail = this.detail();
    if (!detail) return '#';
    const path = entry.path.split('/').map(encodeURIComponent).join('/');
    return `${detail.files_url}${encodeURIComponent(this.revision())}/${path}`;
  }

  protected toggle(path: string, checked: boolean): void {
    this.selected.update((set) => {
      const next = new Set(set);
      if (checked) next.add(path);
      else next.delete(path);
      return next;
    });
  }

  protected async upload(input: HTMLInputElement): Promise<void> {
    const files = Array.from(input.files ?? []);
    input.value = '';
    if (!files.length || this.busy()) return;
    const form = new FormData();
    for (const file of files) {
      // A folder upload names each file by its path inside the folder.
      const relative = (file as File & { webkitRelativePath?: string }).webkitRelativePath;
      form.append('files', file, relative || file.name);
    }
    const query = new URLSearchParams({ revision: this.revision(), directory: this.folder() });
    if (this.message.trim()) query.set('message', this.message.trim());
    this.busy.set(true);
    this.progress.set({ sent: 0, total: files.reduce((sum, file) => sum + file.size, 0) });
    this.feedback.clear();
    try {
      await this.api.upload(`${this.url('/files')}?${query.toString()}`, form, (sent, total) =>
        this.progress.set({ sent, total }),
      );
      this.message = '';
      this.feedback.showSuccess($localize`Uploaded ${files.length}:COUNT: file(s).`);
      await this.load();
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, $localize`The upload failed.`));
    } finally {
      this.progress.set(null);
      this.busy.set(false);
    }
  }

  protected async deleteSelected(): Promise<void> {
    const paths = Array.from(this.selected());
    if (!paths.length || this.busy()) return;
    if (this.confirming() !== 'files') {
      this.confirming.set('files');
      return;
    }
    this.busy.set(true);
    try {
      await this.api.post(this.url('/delete-files'), { revision: this.revision(), paths });
      this.confirming.set('');
      this.feedback.showSuccess($localize`Deleted.`);
      await this.load();
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, $localize`Unable to delete.`));
    } finally {
      this.busy.set(false);
    }
  }

  protected async setPrivate(value: boolean): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    try {
      await this.api.patch(this.url(), { private: value });
      this.feedback.showSuccess(
        value
          ? $localize`This repository is now private.`
          : $localize`This repository is now shared.`,
      );
      await this.load();
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to change the visibility.`),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected async deleteRepository(): Promise<void> {
    if (this.busy()) return;
    if (this.confirming() !== 'repository') {
      this.confirming.set('repository');
      return;
    }
    this.busy.set(true);
    try {
      await this.api.delete(this.url());
      this.feedback.showSuccess($localize`Repository deleted.`);
      await this.router.navigate(['/published']);
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to delete this repository.`),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected cardEntries(
    card: Record<string, unknown> | undefined,
  ): { key: string; value: string }[] {
    if (!card) return [];
    return Object.entries(card).map(([key, value]) => ({
      key,
      value: Array.isArray(value)
        ? value.join(', ')
        : typeof value === 'object'
          ? JSON.stringify(value)
          : String(value),
    }));
  }

  protected typeLabel(): string {
    return this.detail()?.repository.repo_type === 'datasets'
      ? $localize`Dataset`
      : $localize`Model`;
  }

  protected hubAddress(): string {
    const repo = this.detail()?.repository;
    return repo ? repo.repo_id : '';
  }

  protected typeFlag(): string {
    return this.detail()?.repository.repo_type === 'datasets' ? ' --repo-type dataset' : '';
  }
}
