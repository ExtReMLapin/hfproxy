import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import {
  HttpTestingController,
  provideHttpClientTesting,
  TestRequest,
} from '@angular/common/http/testing';
import { AuthService } from './auth.service';
import { FeedbackService } from './feedback.service';

describe('AuthService session handling', () => {
  let auth: AuthService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    auth = TestBed.inject(AuthService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('starts without an authenticated principal', () => {
    expect(auth.principal()).toBeNull();
    expect(auth.busy()).toBeFalsy();
  });

  it('clears the principal when the session expires', () => {
    auth.principal.set({ id: 'u1', username: 'admin', role: 'admin', authenticated: true });
    auth.expireSession();
    expect(auth.principal()).toBeNull();
    expect(auth.error()).toContain('expired');
  });

  it('keeps the principal and reports a failed sign-out', async () => {
    auth.principal.set({ id: 'u1', username: 'admin', role: 'admin', authenticated: true });
    const pending = auth.signOut();
    http.expectOne('/logout').flush(
      { detail: 'CSRF validation failed' },
      {
        status: 403,
        statusText: 'Forbidden',
      },
    );
    await pending;
    expect(auth.principal()?.id).toBe('u1');
    expect(auth.error()).toContain('CSRF');
  });

  it("drops the previous session's error banner when it ends", async () => {
    const feedback = TestBed.inject(FeedbackService);
    auth.principal.set({ id: 'u1', username: 'admin', role: 'admin', authenticated: true });
    feedback.showError('Unable to load your tokens');
    auth.expireSession();
    expect(feedback.error()).toBe('');

    auth.principal.set({ id: 'u1', username: 'admin', role: 'admin', authenticated: true });
    feedback.showError('Unable to load your tokens');
    const pending = auth.signOut();
    http.expectOne('/logout').flush('', { status: 200, statusText: 'OK' });
    // Once signed out the service loads the sign-in options for the login page.
    let providers: TestRequest | undefined;
    await vi.waitFor(() => {
      providers = http.match('/api/v1/auth/providers')[0];
      if (!providers) throw new Error('the sign-in options were not requested yet');
    });
    providers?.flush({ local: true, oidc: false, ldap: false });
    await pending;
    expect(feedback.error()).toBe('');
  });
});
