import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../../core/auth.service';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import { ManagedSettings, SettingsDraft } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

type SettingsSection = 'connections' | 'storage' | 'access' | 'oidc' | 'ldap' | 'cache' | 'observability';

@Component({
  selector: 'app-settings-page',
  standalone: true,
  imports: [FormsModule, IconComponent, PageHeadingComponent],
  templateUrl: './settings-page.html',
  styleUrl: './settings-page.css',
})
export class SettingsPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly auth = inject(AuthService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly section = signal<SettingsSection>('connections');
  protected proxyURLVisible = false;
  protected draft: SettingsDraft | null = null;

  protected readonly sections: { id: SettingsSection; label: string; icon: string }[] = [
    { id: 'connections', label: 'Connections', icon: 'arrow' },
    { id: 'storage', label: 'Object storage', icon: 'database' },
    { id: 'access', label: 'Access & sessions', icon: 'lock' },
    { id: 'oidc', label: 'OIDC / SSO', icon: 'key' },
    { id: 'ldap', label: 'LDAP', icon: 'users' },
    { id: 'cache', label: 'Cache rules', icon: 'layers' },
    { id: 'observability', label: 'Observability', icon: 'layers' },
  ];

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      this.setDraft(await this.api.get<ManagedSettings>('/api/v1/settings'));
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, 'Unable to load instance settings.'),
      );
    } finally {
      this.loading.set(false);
    }
  }

  protected async save(): Promise<void> {
    const draft = this.draft;
    if (!draft) return;
    const payload: Record<string, unknown> = {
      ...draft,
      hf_allowed_hosts: draft.hf_allowed_hosts_text
        .split(',')
        .map((host) => host.trim())
        .filter(Boolean),
    };
    delete payload['hf_allowed_hosts_text'];
    delete payload['hf_proxy_set'];
    delete payload['hf_token_set'];
    delete payload['s3_access_key_set'];
    delete payload['s3_secret_key_set'];
    delete payload['oidc_client_secret_set'];
    delete payload['ldap_bind_password_set'];
    if (!draft.hf_token) delete payload['hf_token'];
    if (!draft.hf_proxy_url) delete payload['hf_proxy_url'];
    if (!draft.s3_access_key) delete payload['s3_access_key'];
    if (!draft.s3_secret_key) delete payload['s3_secret_key'];
    if (!draft.oidc_client_secret) delete payload['oidc_client_secret'];
    if (!draft.ldap_bind_password) delete payload['ldap_bind_password'];
    await this.mutate(async () => {
      const saved = await this.api.put<ManagedSettings>('/api/v1/settings', payload);
      this.setDraft(saved);
      await this.auth.refreshProviders();
      this.feedback.showSuccess('Settings saved and applied. Secret values are write-only.');
    });
  }

  protected async testHub(): Promise<void> {
    const settings = this.draft;
    if (!settings) return;
    await this.mutate(async () => {
      await this.api.post('/api/v1/settings/test-hub', {
        hf_base_url: settings.hf_base_url,
        hf_proxy_url: settings.hf_proxy_url,
        clear_hf_proxy: settings.clear_hf_proxy,
        hf_token: settings.hf_token,
        clear_hf_token: settings.clear_hf_token,
      });
      this.feedback.showSuccess('Hugging Face Hub connection succeeded.');
    });
  }

  protected async testS3(): Promise<void> {
    const settings = this.draft;
    if (!settings) return;
    await this.mutate(async () => {
      await this.api.post('/api/v1/settings/test-s3', {
        s3_endpoint: settings.s3_endpoint,
        s3_region: settings.s3_region,
        s3_bucket: settings.s3_bucket,
        s3_prefix: settings.s3_prefix,
        s3_force_path_style: settings.s3_force_path_style,
        s3_server_side_encryption: settings.s3_server_side_encryption,
        s3_access_key: settings.s3_access_key,
        s3_secret_key: settings.s3_secret_key,
        clear_s3_credentials: settings.clear_s3_credentials,
      });
      this.feedback.showSuccess('S3-compatible storage connection succeeded.');
    });
  }

  protected async refresh(): Promise<void> {
    await this.load();
  }

  private setDraft(settings: ManagedSettings): void {
    this.draft = {
      ...settings,
      hf_allowed_hosts_text: settings.hf_allowed_hosts.join(', '),
      hf_proxy_url: '',
      clear_hf_proxy: false,
      hf_token: '',
      clear_hf_token: false,
      s3_access_key: '',
      s3_secret_key: '',
      clear_s3_credentials: false,
      oidc_client_secret: '',
      clear_oidc_client_secret: false,
      ldap_bind_password: '',
      clear_ldap_bind_password: false,
    };
  }

  private async mutate(action: () => Promise<void>): Promise<void> {
    this.busy.set(true);
    this.feedback.clear();
    try {
      await action();
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, 'Unable to apply these settings.'),
      );
    } finally {
      this.busy.set(false);
    }
  }
}
