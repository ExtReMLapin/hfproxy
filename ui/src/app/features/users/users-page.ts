import { CommonModule } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule, NgForm } from '@angular/forms';
import { AuthService } from '../../core/auth.service';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import { AdminUser } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-users-page',
  standalone: true,
  imports: [CommonModule, FormsModule, IconComponent, PageHeadingComponent],
  templateUrl: './users-page.html',
})
export class UsersPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly auth = inject(AuthService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly users = signal<AdminUser[]>([]);
  private readonly savedValues = new Map<string, { role: string; status: string }>();
  // Quotas typed in the table, in GiB, by user id; absent until edited.
  private readonly quotaEdits = new Map<string, string>();
  protected draft = { username: '', email: '', display_name: '', password: '', role: 'reader' };

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      this.users.set(await this.api.listAll<AdminUser>('/api/v1/users'));
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to load users.`),
      );
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(form?: NgForm): Promise<void> {
    if (form && !form.valid) {
      form.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    try {
      await this.api.post('/api/v1/users', this.draft);
      this.draft = { username: '', email: '', display_name: '', password: '', role: 'reader' };
      await this.load();
      this.feedback.showSuccess($localize`User account created.`);
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to create this user.`),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected async save(user: AdminUser): Promise<void> {
    const previous = this.savedValues.get(user.id) ?? { role: user.role, status: user.status };
    this.busy.set(true);
    const quota = this.quotaEdits.has(user.id)
      ? this.quotaBytes(this.quotaEdits.get(user.id)!)
      : undefined;
    if (quota === 'invalid') {
      this.feedback.showError(
        $localize`The quota must be a number of GiB, 0 for unlimited, or empty for the default.`,
      );
      this.busy.set(false);
      return;
    }
    try {
      await this.api.patch(`/api/v1/users/${encodeURIComponent(user.id)}`, {
        role: user.role,
        status: user.status,
        ...(quota === undefined ? {} : { daily_quota_bytes: quota }),
      });
      if (quota !== undefined) user.daily_quota_bytes = quota;
      this.quotaEdits.delete(user.id);
      this.savedValues.delete(user.id);
      this.feedback.showSuccess($localize`Updated ${user.username}:USERNAME:.`);
    } catch (error) {
      user.role = previous.role;
      user.status = previous.status;
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to update this user.`),
      );
    } finally {
      this.busy.set(false);
    }
  }

  protected setRole(user: AdminUser, role: string): void {
    this.savedValues.set(
      user.id,
      this.savedValues.get(user.id) ?? { role: user.role, status: user.status },
    );
    user.role = role;
  }

  protected setStatus(user: AdminUser, status: string): void {
    this.savedValues.set(
      user.id,
      this.savedValues.get(user.id) ?? { role: user.role, status: user.status },
    );
    user.status = status;
  }

  /** What the quota field shows: GiB, empty when the user follows the default. */
  protected quotaText(user: AdminUser): string {
    const edited = this.quotaEdits.get(user.id);
    if (edited !== undefined) return edited;
    if (user.daily_quota_bytes === null || user.daily_quota_bytes === undefined) return '';
    return String(Math.round((user.daily_quota_bytes / 2 ** 30) * 100) / 100);
  }

  protected setQuota(user: AdminUser, text: string | number | null): void {
    this.quotaEdits.set(user.id, text === null ? '' : String(text));
  }

  /** GiB typed by the administrator to bytes: null for the default, 'invalid' for a bad entry. */
  private quotaBytes(text: string): number | null | 'invalid' {
    if (text.trim() === '') return null;
    const gib = Number(text);
    if (!Number.isFinite(gib) || gib < 0) return 'invalid';
    return Math.round(gib * 2 ** 30);
  }

  protected get currentUserId(): string {
    return this.auth.principal()?.id ?? '';
  }
}
