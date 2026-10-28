import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ManagedSettings } from '../../core/admin.types';
import { SettingsPageComponent } from './settings-page';

const settings: ManagedSettings = {
  public_url: 'https://hf.example.com',
  hf_base_url: 'https://huggingface.co',
  hf_proxy_set: false,
  hf_allowed_hosts: ['huggingface.co'],
  hf_timeout: '2m0s',
  hf_token_set: true,
  s3_endpoint: '',
  s3_region: 'us-east-1',
  s3_bucket: 'bucket',
  s3_prefix: 'hfproxy',
  s3_force_path_style: true,
  s3_server_side_encryption: true,
  s3_access_key_set: true,
  s3_secret_key_set: true,
  auth_mode: 'local',
  anonymous_public_read: false,
  session_idle_ttl: '8h0m0s',
  session_absolute_ttl: '24h0m0s',
  cache_max_file_size: 107374182400,
  cache_max_total_size: 5368709120,
  cache_ref_ttl: '1m0s',
  cache_negative_ttl: '30s',
  cache_gc_grace: '24h0m0s',
  oidc_issuer: '',
  oidc_client_id: '',
  oidc_client_secret_set: false,
  oidc_redirect_url: '',
  oidc_auto_provision: false,
  ldap_url: '',
  ldap_starttls: true,
  ldap_bind_dn: '',
  ldap_bind_password_set: false,
  ldap_base_dn: '',
  ldap_user_filter: '(uid={username})',
  ldap_auto_provision: false,
  metrics_enabled: false,
  metrics_path: '/metrics',
  tracing_enabled: false,
  tracing_service_name: 'hfproxy',
  tracing_sampling_ratio: 0.1,
  otlp_endpoint: '',
  cache_inventory_refresh: '5m0s',
} as ManagedSettings;

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

type SettingsPageInternals = {
  draft: (ManagedSettings & Record<string, unknown>) | null;
  save(): Promise<void>;
};

describe('Settings page', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    http = TestBed.inject(HttpTestingController);
  });

  it('shows the total cache size limit and sends a changed value back to the server', async () => {
    const page = TestBed.runInInjectionContext(() => new SettingsPageComponent());
    const internals = page as unknown as SettingsPageInternals;
    page.ngOnInit();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();

    expect(internals.draft?.cache_max_total_size).toBe(5368709120);

    internals.draft!.cache_max_total_size = 107374182400;
    const saving = internals.save();
    await flushMicrotasks();
    const put = http.expectOne((request) => request.method === 'PUT' && request.url === '/api/v1/settings');
    const body = put.request.body as Record<string, unknown>;
    expect(body['cache_max_total_size']).toBe(107374182400);
    // Read-only flags and the draft helpers never go back to the server.
    for (const key of Object.keys(body)) expect(key.endsWith('_set')).toBe(false);
    expect(body['hf_allowed_hosts_text']).toBeUndefined();
    put.flush({ ...settings, cache_max_total_size: 107374182400 });
    await flushMicrotasks();
    http.match('/api/v1/auth/providers').forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
    expect(internals.draft?.cache_max_total_size).toBe(107374182400);
  });

  it('keeps 0 as "no limit" instead of dropping it from the request', async () => {
    const page = TestBed.runInInjectionContext(() => new SettingsPageComponent());
    const internals = page as unknown as SettingsPageInternals;
    page.ngOnInit();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();

    internals.draft!.cache_max_total_size = 0;
    const saving = internals.save();
    await flushMicrotasks();
    const put = http.expectOne((request) => request.method === 'PUT');
    expect((put.request.body as Record<string, unknown>)['cache_max_total_size']).toBe(0);
    put.flush({ ...settings, cache_max_total_size: 0 });
    await flushMicrotasks();
    http.match('/api/v1/auth/providers').forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
  });

  it('renders the limit as an editable number field in the cache rules', async () => {
    const fixture = TestBed.createComponent(SettingsPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();
    const internals = fixture.componentInstance as unknown as { section: { set(value: string): void } };
    internals.section.set('cache');
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();

    const field = fixture.nativeElement.querySelector('input[name="maxTotalSize"]') as HTMLInputElement | null;
    expect(field).not.toBeNull();
    expect(field!.type).toBe('number');
    expect(field!.min).toBe('0');
    expect(field!.value).toBe('5368709120');
    expect(fixture.nativeElement.textContent).toContain('Pinned files are never removed');
  });
});
