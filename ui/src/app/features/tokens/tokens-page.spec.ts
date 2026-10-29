import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TokensPageComponent } from './tokens-page';
import { AdminShellComponent } from '../../layout/admin-shell';
import { AuthService } from '../../core/auth.service';
import { provideRouter } from '@angular/router';

describe('Personal access tokens', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('lets a reader find the tokens page without exposing admin navigation', () => {
    TestBed.inject(AuthService).principal.set({ role: 'reader', username: 'alice' } as any);
    const fixture = TestBed.createComponent(AdminShellComponent);
    fixture.detectChanges();
    const links = Array.from(
      fixture.nativeElement.querySelectorAll('nav a'),
    ) as HTMLAnchorElement[];
    expect(links.some((link) => link.textContent?.includes('Access tokens'))).toBe(true);
    expect(links.some((link) => link.textContent?.includes('Users'))).toBe(false);
    fixture.destroy();
  });

  it('creates from the form, displays the secret once and requires confirmation before revoking', async () => {
    const fixture = TestBed.createComponent(TokensPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/me/tokens').flush({ items: [] });
    http.expectOne('/api/v1/settings/public').flush({ custom_repositories: false });
    await fixture.whenStable();
    fixture.detectChanges();
    const form = fixture.nativeElement.querySelector('form') as HTMLFormElement;
    form.dispatchEvent(new Event('submit'));
    http.expectNone((req) => req.method === 'POST');
    const name = form.querySelector('input')!;
    name.value = 'Workstation';
    name.dispatchEvent(new Event('input'));
    await fixture.whenStable();
    fixture.detectChanges();
    form.dispatchEvent(new Event('submit'));
    const create = http.expectOne((req) => req.method === 'POST');
    expect(create.request.body).toEqual({
      name: 'Workstation',
      expires_in_days: 90,
      scope: 'read',
    });
    const token = {
      id: 't1',
      name: 'Workstation',
      prefix: 'hfpx_prefix',
      scopes: ['read'],
      expires_at: null,
      last_used_at: null,
    };
    create.flush({ ...token, token: 'hfpx_secret_shown_once' });
    await Promise.resolve();
    http.expectOne('/api/v1/me/tokens').flush({ items: [token] });
    await fixture.whenStable();
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('#new-token').value).toBe('hfpx_secret_shown_once');
    fixture.nativeElement.querySelector('[role="status"] button').click();
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('#new-token')).toBeNull();
    fixture.nativeElement.querySelector('tbody button').click();
    fixture.detectChanges();
    http.expectNone((req) => req.method === 'DELETE');
    fixture.nativeElement.querySelector('tbody button').click();
    http.expectOne((req) => req.method === 'DELETE' && req.url.endsWith('/t1')).flush(null);
    await fixture.whenStable();
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('tbody').textContent).not.toContain('Workstation');
    fixture.destroy();
  });

  it('offers a publish token only when publishing is on', async () => {
    const fixture = TestBed.createComponent(TokensPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/me/tokens').flush({ items: [] });
    http.expectOne('/api/v1/settings/public').flush({ custom_repositories: true });
    await fixture.whenStable();
    fixture.detectChanges();
    const select = fixture.nativeElement.querySelector('select[name="scope"]') as HTMLSelectElement;
    expect(select).not.toBeNull();
    await fixture.whenStable(); // the form registers its controls after the first render
    select.value = 'publish';
    select.dispatchEvent(new Event('change'));
    const form = fixture.nativeElement.querySelector('form') as HTMLFormElement;
    const name = form.querySelector('input')!;
    name.value = 'Uploader';
    name.dispatchEvent(new Event('input'));
    await fixture.whenStable();
    fixture.detectChanges();
    form.dispatchEvent(new Event('submit'));
    const create = http.expectOne((req) => req.method === 'POST');
    expect(create.request.body).toEqual({
      name: 'Uploader',
      expires_in_days: 90,
      scope: 'publish',
    });
    create.flush({
      id: 't2',
      name: 'Uploader',
      prefix: 'hfpx_x',
      scopes: ['publish'],
      expires_at: null,
      last_used_at: null,
      token: 'hfpx_secret',
    });
    await Promise.resolve();
    http.expectOne('/api/v1/me/tokens').flush({ items: [] });
    await fixture.whenStable();
    fixture.destroy();
  });
});
