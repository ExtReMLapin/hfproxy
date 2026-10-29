import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { AdminApiService } from '../../core/admin-api.service';
import { PublishedDetailPageComponent } from './published-detail-page';

const repository = {
  id: 'r1',
  repo_type: 'models',
  repo_id: 'alice/tiny',
  namespace: 'alice',
  name: 'tiny',
  private: false,
  owner: 'alice',
  bytes: 2048,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-02T00:00:00Z',
  can_write: true,
};
const detail = {
  repository,
  revision: 'main',
  branches: ['main', 'dev'],
  tags: ['v1'],
  head: { sha: 'a'.repeat(40), summary: 'first', author: 'alice', date: '2026-01-02T00:00:00Z' },
  files_url: 'http://proxy/alice/tiny/resolve/',
  readme: {
    content: '# Tiny\n\nA <b>test</b> model.',
    card: { license: 'mit', tags: ['a', 'b'] },
    size: 30,
  },
};
const tree = {
  items: [
    { name: 'sub', path: 'sub', type: 'directory', size: 0, lfs: false },
    { name: 'big.bin', path: 'big.bin', type: 'file', size: 4096, lfs: true },
  ],
  truncated: false,
};

describe('PublishedDetailPageComponent', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: { snapshot: { paramMap: convertToParamMap({ id: 'r1' }) } },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  async function open(overrides: object = {}) {
    const fixture = TestBed.createComponent(PublishedDetailPageComponent);
    fixture.detectChanges();
    http
      .expectOne((r) => r.url === '/api/v1/published/r1?revision=main')
      .flush({ ...detail, ...overrides });
    await fixture.whenStable();
    http.expectOne((r) => r.url.startsWith('/api/v1/published/r1/tree?')).flush(tree);
    await fixture.whenStable();
    fixture.detectChanges();
    return fixture;
  }

  it('shows the files with download links and the large-file marker', async () => {
    const fixture = await open();
    const text = fixture.nativeElement.textContent as string;
    expect(text).toContain('alice/tiny');
    expect(text).toContain('big.bin');
    expect(text).toContain('LFS');
    expect(text).toContain('4.0 KiB');
    const link = fixture.nativeElement.querySelector('a[href*="resolve"]') as HTMLAnchorElement;
    expect(link.href).toBe('http://proxy/alice/tiny/resolve/main/big.bin');
  });

  it('shows the card as text, never as markup', async () => {
    const fixture = await open();
    const tabs = Array.from(
      fixture.nativeElement.querySelectorAll('nav button'),
    ) as HTMLButtonElement[];
    tabs.find((tab) => tab.textContent?.includes('Card'))!.click();
    fixture.detectChanges();
    expect(fixture.nativeElement.textContent).toContain('license:');
    expect(fixture.nativeElement.textContent).toContain('A <b>test</b> model.');
    expect(fixture.nativeElement.querySelector('app-markdown b')).toBeNull();
  });

  it('uploads files as one multipart form into the folder shown', async () => {
    const fixture = await open();
    const upload = vi
      .spyOn(TestBed.inject(AdminApiService), 'upload')
      .mockImplementation(async (_url, form, progress) => {
        progress(5, 10);
        expect(form.getAll('files').length).toBe(2);
        return { commit: 'c', files: 2 };
      });
    const input = {
      files: [new File(['a'], 'a.txt'), new File(['b'], 'b.txt')],
      value: 'x',
    } as unknown as HTMLInputElement;
    const done = (
      fixture.componentInstance as unknown as { upload(i: HTMLInputElement): Promise<void> }
    ).upload(input);
    for (let i = 0; i < 10; i++) await Promise.resolve();
    // The page reloads what it shows once the commit is made.
    http.expectOne((r) => r.url.startsWith('/api/v1/published/r1?')).flush(detail);
    for (let i = 0; i < 10; i++) await Promise.resolve();
    http.expectOne((r) => r.url.startsWith('/api/v1/published/r1/tree?')).flush(tree);
    await done;
    expect(upload).toHaveBeenCalledTimes(1);
    expect(upload.mock.calls[0][0]).toContain('/api/v1/published/r1/files?');
    expect(upload.mock.calls[0][0]).toContain('revision=main');
    expect(input.value).toBe('');
  });

  it('asks for confirmation before deleting the selected files', async () => {
    const fixture = await open();
    const checkbox = fixture.nativeElement.querySelector(
      'input[type="checkbox"]',
    ) as HTMLInputElement;
    checkbox.click();
    fixture.detectChanges();
    const remove = Array.from(fixture.nativeElement.querySelectorAll('button')).find((b) =>
      (b as HTMLElement).textContent?.includes('Delete selected'),
    ) as HTMLButtonElement;
    remove.click();
    http.expectNone((r) => r.method === 'POST');
    fixture.detectChanges();
    const confirm = Array.from(fixture.nativeElement.querySelectorAll('button')).find((b) =>
      (b as HTMLElement).textContent?.includes('Confirm: delete'),
    ) as HTMLButtonElement;
    confirm.click();
    const post = http.expectOne(
      (r) => r.method === 'POST' && r.url === '/api/v1/published/r1/delete-files',
    );
    expect(post.request.body).toEqual({ revision: 'main', paths: ['sub'] });
    post.flush({ commit: 'c' });
    for (let i = 0; i < 10; i++) await Promise.resolve();
    http.match(() => true);
  });

  it('hides the editing controls from someone who cannot write', async () => {
    const fixture = await open({ repository: { ...repository, can_write: false } });
    const text = fixture.nativeElement.textContent as string;
    expect(text).not.toContain('Add files');
    expect(text).not.toContain('Make private');
    expect(fixture.nativeElement.querySelector('input[type="checkbox"]')).toBeNull();
  });
});
