import { HttpClient } from '@angular/common/http';
import { inject, Injectable, signal } from '@angular/core';
import { HttpErrorResponse } from '@angular/common/http';
import { firstValueFrom } from 'rxjs';
import { AdminApiService } from './admin-api.service';
import { FeedbackService } from './feedback.service';
import { AuthProviders, FirstAdminInput, Principal } from './admin.types';

@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly http = inject(HttpClient);
  private readonly api = inject(AdminApiService);
  private readonly feedback = inject(FeedbackService);
  private initialization?: Promise<void>;

  readonly principal = signal<Principal | null>(null);
  readonly providers = signal<AuthProviders>({ local: true, oidc: false, ldap: false });
  /** True while the instance has no administrator and offers to create one. */
  readonly setupRequired = signal(false);
  readonly loading = signal(true);
  readonly busy = signal(false);
  readonly error = signal('');

  initialize(): Promise<void> {
    this.initialization ??= this.loadPrincipal();
    return this.initialization;
  }

  private async loadPrincipal(): Promise<void> {
    this.loading.set(true);
    try {
      this.principal.set(await this.api.get<Principal>('/api/v1/me'));
      await this.loadProviders();
    } catch {
      this.principal.set(null);
      await this.loadProviders();
      await this.loadSetupStatus();
    } finally {
      this.loading.set(false);
    }
  }

  private async loadSetupStatus(): Promise<void> {
    try {
      this.setupRequired.set(
        (await this.api.get<{ required: boolean }>('/api/v1/setup/status')).required,
      );
    } catch {
      this.setupRequired.set(false);
    }
  }

  /**
   * Creates the first administrator. The service signs the new administrator
   * in, so the console opens as soon as the principal is loaded.
   */
  async createFirstAdmin(input: FirstAdminInput): Promise<void> {
    this.error.set('');
    this.busy.set(true);
    try {
      await this.api.post('/api/v1/setup', input);
      this.setupRequired.set(false);
      this.principal.set(await this.api.get<Principal>('/api/v1/me'));
    } catch (error) {
      if (error instanceof HttpErrorResponse && error.status === 409) {
        // Someone else finished the setup first: the sign-in page takes over.
        this.setupRequired.set(false);
      }
      this.error.set(
        AdminApiService.errorMessage(error, $localize`The administrator could not be created.`),
      );
    } finally {
      this.busy.set(false);
    }
  }

  async signIn(username: string, password: string): Promise<void> {
    this.error.set('');
    this.busy.set(true);
    try {
      const html = await firstValueFrom(this.http.get('/login', { responseType: 'text' }));
      const loginDocument = new DOMParser().parseFromString(html, 'text/html');
      const csrf = loginDocument.querySelector<HTMLInputElement>('input[name="csrf_token"]')?.value;
      if (!csrf)
        throw new Error(
          $localize`Could not start a secure sign-in. Refresh the page and try again.`,
        );
      const form = new URLSearchParams({ csrf_token: csrf, username, password });
      await firstValueFrom(
        this.http.post('/login', form.toString(), {
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          responseType: 'text',
        }),
      );
      const me = await this.api.get<Principal>('/api/v1/me');
      this.principal.set(me);
    } catch (error) {
      this.error.set(
        AdminApiService.errorMessage(
          error,
          $localize`Sign-in failed. Check your credentials and try again.`,
        ),
      );
    } finally {
      this.busy.set(false);
    }
  }

  startSSO(): void {
    window.location.assign('/auth/login');
  }

  async refreshProviders(): Promise<void> {
    await this.loadProviders();
  }

  async signOut(): Promise<void> {
    this.error.set('');
    this.busy.set(true);
    try {
      const form = new URLSearchParams({ csrf_token: this.cookie('hfproxy_csrf') });
      await firstValueFrom(
        this.http.post('/logout', form.toString(), {
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          responseType: 'text',
        }),
      );
      this.principal.set(null);
      this.feedback.clear();
    } catch (error) {
      if (error instanceof HttpErrorResponse && error.status === 401) {
        this.principal.set(null);
        this.feedback.clear();
      } else {
        const message = AdminApiService.errorMessage(
          error,
          $localize`Sign-out failed. Please retry.`,
        );
        this.error.set(message);
        this.feedback.showError(message);
      }
    } finally {
      this.busy.set(false);
      if (!this.principal()) await this.loadProviders();
    }
  }

  expireSession(): void {
    this.principal.set(null);
    // A message about the previous session must not outlive it.
    this.feedback.clear();
    this.error.set($localize`Your session has expired. Please sign in again.`);
  }

  private async loadProviders(): Promise<void> {
    try {
      this.providers.set(await this.api.get<AuthProviders>('/api/v1/auth/providers'));
    } catch {
      this.providers.set({ local: true, oidc: false, ldap: false });
    }
  }

  private cookie(name: string): string {
    const prefix = `${name}=`;
    return (
      document.cookie
        .split(';')
        .map((part) => part.trim())
        .find((part) => part.startsWith(prefix))
        ?.slice(prefix.length) ?? ''
    );
  }
}
