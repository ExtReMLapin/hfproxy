import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { AuthService } from '../../core/auth.service';
import { SetupPageComponent } from './setup-page';

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

interface SetupInternals {
  form: {
    setupCode: string;
    username: string;
    displayName: string;
    email: string;
    password: string;
    confirmation: string;
  };
  validate(): string;
  submit(): Promise<void>;
}

describe('First administrator setup', () => {
  let http: HttpTestingController;
  let auth: AuthService;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
    auth = TestBed.inject(AuthService);
  });
  afterEach(() => http.verify());

  function page(): SetupInternals {
    const component = TestBed.runInInjectionContext(() => new SetupPageComponent());
    const internals = component as unknown as SetupInternals;
    Object.assign(internals.form, {
      setupCode: ' code-from-the-log ',
      username: ' root ',
      displayName: 'Root',
      email: 'root@example.com',
      password: 'a-long-enough-password',
      confirmation: 'a-long-enough-password',
    });
    return internals;
  }

  it('checks the form before anything is sent', async () => {
    const setup = page();
    expect(setup.validate()).toBe('');

    setup.form.setupCode = '  ';
    expect(setup.validate()).toContain('setup code');
    setup.form.setupCode = 'x';
    setup.form.username = 'no spaces';
    expect(setup.validate()).toContain('username');
    setup.form.username = 'root';
    setup.form.password = setup.form.confirmation = 'short';
    expect(setup.validate()).toContain('at least 12');
    setup.form.password = 'a-long-enough-password';
    setup.form.confirmation = 'another-long-password';
    expect(setup.validate()).toContain('do not match');

    await setup.submit();
    http.expectNone('/api/v1/setup');
  });

  it('creates the administrator and opens the console signed in', async () => {
    auth.setupRequired.set(true);
    const setup = page();
    const submitting = setup.submit();
    await flush();
    const post = http.expectOne(
      (request) => request.method === 'POST' && request.url === '/api/v1/setup',
    );
    expect(post.request.body).toEqual({
      setup_code: 'code-from-the-log',
      username: 'root',
      display_name: 'Root',
      email: 'root@example.com',
      password: 'a-long-enough-password',
    });
    post.flush(
      { id: 'u1', username: 'root', role: 'admin' },
      { status: 201, statusText: 'Created' },
    );
    await flush();
    http
      .expectOne('/api/v1/me')
      .flush({ id: 'u1', username: 'root', role: 'admin', authenticated: true });
    await submitting;
    expect(auth.setupRequired()).toBe(false);
    expect(auth.principal()?.role).toBe('admin');
    expect(auth.error()).toBe('');
  });

  it('shows the reason when the code is wrong and clears the passwords', async () => {
    auth.setupRequired.set(true);
    const setup = page();
    const submitting = setup.submit();
    await flush();
    http
      .expectOne('/api/v1/setup')
      .flush(
        { title: 'Invalid setup code', detail: 'Enter the setup code printed in the log.' },
        { status: 403, statusText: 'Forbidden' },
      );
    await submitting;
    expect(auth.error()).toContain('setup code');
    expect(auth.setupRequired()).toBe(true);
    expect(auth.principal()).toBeNull();
    expect(setup.form.password).toBe('');
    expect(setup.form.confirmation).toBe('');
  });

  it('hands over to the sign-in page when someone else finished the setup first', async () => {
    auth.setupRequired.set(true);
    const setup = page();
    const submitting = setup.submit();
    await flush();
    http.expectOne('/api/v1/setup').flush(
      {
        title: 'Setup already completed',
        detail: 'An administrator already exists. Sign in instead.',
      },
      { status: 409, statusText: 'Conflict' },
    );
    await submitting;
    expect(auth.setupRequired()).toBe(false);
  });
});

describe('Setup status', () => {
  it('is read when nobody is signed in, and stays off when the check fails', async () => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    const http = TestBed.inject(HttpTestingController);
    const auth = TestBed.inject(AuthService);

    const initializing = auth.initialize();
    http.expectOne('/api/v1/me').flush({}, { status: 401, statusText: 'Unauthorized' });
    await flush();
    http.expectOne('/api/v1/auth/providers').flush({ local: true, oidc: false, ldap: false });
    await flush();
    http.expectOne('/api/v1/setup/status').flush({ required: true });
    await initializing;
    expect(auth.setupRequired()).toBe(true);
    expect(auth.loading()).toBe(false);
    http.verify();
  });

  it('is not asked for a signed-in user', async () => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    const http = TestBed.inject(HttpTestingController);
    const auth = TestBed.inject(AuthService);
    const initializing = auth.initialize();
    http.expectOne('/api/v1/me').flush({ id: 'u1', username: 'root', role: 'admin' });
    await flush();
    http.expectOne('/api/v1/auth/providers').flush({ local: true, oidc: false, ldap: false });
    await initializing;
    expect(auth.setupRequired()).toBe(false);
    http.expectNone('/api/v1/setup/status');
    http.verify();
  });
});

describe('Setup form feedback', () => {
  it('replaces the message of an earlier attempt with the current problem', async () => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    const component = TestBed.runInInjectionContext(() => new SetupPageComponent());
    const internals = component as unknown as SetupInternals & { problem: () => string };
    Object.assign(internals.form, {
      setupCode: 'c',
      username: 'root',
      password: 'a-long-enough-password',
      confirmation: 'other-long-password!',
    });
    await internals.submit();
    expect(internals.problem()).toContain('do not match');
    internals.form.confirmation = '';
    await internals.submit();
    expect(internals.problem()).toContain('do not match');
    internals.form.setupCode = '';
    await internals.submit();
    expect(internals.problem()).toContain('setup code');
  });
});
