import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { Router, provideRouter } from '@angular/router';
import { PublishedPageComponent } from './published-page';

const repo = {
  id: 'r1',
  repo_type: 'models',
  repo_id: 'alice/tiny',
  namespace: 'alice',
  name: 'tiny',
  private: true,
  owner: 'alice',
  bytes: 5 * 1024 * 1024,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-02T00:00:00Z',
  can_write: true,
};

describe('PublishedPageComponent', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  async function open(list: object) {
    const fixture = TestBed.createComponent(PublishedPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/published').flush(list);
    await fixture.whenStable();
    fixture.detectChanges();
    await fixture.whenStable();
    return fixture;
  }

  it('lists what is published and offers to create a repository', async () => {
    const fixture = await open({
      items: [repo],
      enabled: true,
      namespaces: ['alice', 'research'],
      max_file: 1 << 30,
      quota: 0,
    });
    const text = fixture.nativeElement.textContent as string;
    expect(text).toContain('alice/tiny');
    expect(text).toContain('5.0 MiB');
    expect(text).toContain('Private');
    expect(fixture.nativeElement.querySelector('form')).not.toBeNull();
    const owners = Array.from(
      fixture.nativeElement.querySelectorAll('select[name="namespace"] option'),
    ).map((o) => (o as HTMLOptionElement).value);
    expect(owners).toEqual(['alice', 'research']);
  });

  it('says so when publishing is off and hides the form', async () => {
    const fixture = await open({
      items: [],
      enabled: false,
      namespaces: ['alice'],
      max_file: 1,
      quota: 0,
    });
    expect(fixture.nativeElement.textContent).toContain('Publishing is turned off');
    expect(fixture.nativeElement.querySelector('form')).toBeNull();
  });

  it('creates a repository and opens it', async () => {
    const fixture = await open({
      items: [],
      enabled: true,
      namespaces: ['alice'],
      max_file: 1,
      quota: 0,
    });
    const navigate = vi.spyOn(TestBed.inject(Router), 'navigate').mockResolvedValue(true);
    const form = fixture.nativeElement.querySelector('form') as HTMLFormElement;
    const name = form.querySelector('input[name="repoName"]') as HTMLInputElement;
    name.value = 'my-model';
    name.dispatchEvent(new Event('input'));
    await fixture.whenStable();
    fixture.detectChanges();
    form.dispatchEvent(new Event('submit'));
    const create = http.expectOne(
      (request) => request.method === 'POST' && request.url === '/api/v1/published',
    );
    expect(create.request.body).toEqual({
      repo_type: 'model',
      namespace: 'alice',
      name: 'my-model',
      private: false,
    });
    create.flush({ ...repo, id: 'new-id' });
    await fixture.whenStable();
    expect(navigate).toHaveBeenCalledWith(['/published', 'new-id']);
  });
});
