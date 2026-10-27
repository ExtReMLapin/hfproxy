import { CommonModule } from '@angular/common';
import { Component, OnInit, inject, signal } from '@angular/core';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import { AuditItem } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-audit-page',
  standalone: true,
  imports: [CommonModule, IconComponent, PageHeadingComponent],
  templateUrl: './audit-page.html',
})
export class AuditPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly events = signal<AuditItem[]>([]);

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      this.events.set(
        (await this.api.get<{ items: AuditItem[] }>('/api/v1/audit-events?limit=100')).items,
      );
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to load audit events.'));
    } finally {
      this.loading.set(false);
    }
  }
}
