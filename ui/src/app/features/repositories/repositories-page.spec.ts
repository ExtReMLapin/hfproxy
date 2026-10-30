import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { PreloadJob, Repository } from '../../core/admin.types';
import { RepositoriesPageComponent } from './repositories-page';

const repo = {
  id: 'r1',
  repo_type: 'models',
  repo_id: 'org/model',
  access_policy: 'public',
  updated_at: '2026-01-01T00:00:00Z',
} as Repository;

const failed: PreloadJob = {
  id: 'old',
  repo_type: 'models',
  repo_id: 'org/model',
  status: 'failed',
  files_total: 0,
  files_processed: 0,
  failed_files: 0,
  error: 'unable to list repository files from Hugging Face',
  started_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
} as PreloadJob;

describe('RepositoriesPageComponent preload', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
  });

  it('replaces the previous failure with the new attempt as soon as it starts', async () => {
    const fixture = TestBed.createComponent(RepositoriesPageComponent);
    fixture.detectChanges();
    const internals = fixture.componentInstance as unknown as {
      preloadJobs: { set(jobs: PreloadJob[]): void; (): PreloadJob[] };
      preloadFailed(repo: Repository): PreloadJob | undefined;
      preloadFor(repo: Repository): PreloadJob | undefined;
      preloadStarting(repo: Repository): boolean;
      preload(repo: Repository): Promise<void>;
    };
    internals.preloadJobs.set([failed]);
    expect(internals.preloadFailed(repo)?.error).toContain('unable to list');

    const pending = internals.preload(repo);
    expect(internals.preloadStarting(repo)).toBe(true);
    const post = http.expectOne(
      (request) => request.method === 'POST' && request.url === '/api/v1/repos/r1/preload',
    );
    post.flush({ ...failed, id: 'new', status: 'queued', error: undefined });
    await pending;

    expect(internals.preloadFailed(repo)).toBeUndefined();
    expect(internals.preloadFor(repo)?.id).toBe('new');
    expect(internals.preloadStarting(repo)).toBe(false);
    http.match(() => true);
  });

  it('sends the exemption from the safe formats along with the policy', () => {
    const fixture = TestBed.createComponent(RepositoriesPageComponent);
    fixture.detectChanges();
    const internals = fixture.componentInstance as unknown as {
      savePolicy(repo: Repository): Promise<void>;
    };
    void internals.savePolicy({ ...repo, allow_unsafe_formats: true });
    const patch = http.expectOne(
      (request) => request.method === 'PATCH' && request.url === '/api/v1/repos/r1',
    );
    expect(patch.request.body).toEqual({ access_policy: 'public', allow_unsafe_formats: true });
    patch.flush({ id: 'r1' });
    http.match(() => true);
  });
});
