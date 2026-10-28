# Hugging Face Compatibility Audit

This audit describes the support actually exposed by hfproxy's code. It is not
a promise of complete compatibility with every `huggingface_hub` version. The
tests behind this matrix run with `go test ./...`; scenarios requiring
PostgreSQL and S3 are marked as integration tests and enabled with
`HFPROXY_TEST_DATABASE_URL`.

## Functional matrix

| Hugging Face feature | hfproxy route | Types | Methods | Support | Evidence / notes |
| --- | --- | --- | --- | --- | --- |
| Download a file | `/{repo_id}/resolve/{revision}/{path}` | models | GET, HEAD | Supported | PostgreSQL cache + S3 blob; branch/tag/SHA revisions |
| Download a typed repository file | `/{type}/{repo_id}/resolve/{revision}/{path}` | models, datasets, spaces | GET, HEAD | Supported | Nested paths; `type` is limited to these three values |
| Dataset Parquet/shard file | `/datasets/{repo_id}/resolve/{revision}/{path}` | datasets | GET, HEAD | Supported | Parquet and shard files are cached as opaque, integrity-checked blobs |
| `snapshot_download` protocol | Repository metadata/tree followed by resolve routes | models | GET, HEAD | Supported | Supports branch, tag, and commit revisions; filtering is performed client-side |
| Dataset `snapshot_download` protocol | Dataset metadata/tree followed by typed resolve routes | datasets | GET, HEAD | Supported | Supports snapshots, nested shard paths, and branch/tag/commit revisions |
| Transformers `from_pretrained` protocol | Repository metadata followed by resolve routes | models | GET, HEAD | Supported | Config, tokenizer, processor, and weight files use the same read path |
| Resume a download | Same route with `Range` | All types above | GET | Supported with conditions | Single ranges; `206`/`416`; multipart ranges rejected; persisted resume for known sizes |
| Conditional requests | Same route with `If-None-Match`, `If-Range` | All types above | GET, HEAD | Supported | ETag preserved; `304`, `206`, and `416` include the relevant validators/range headers |
| Repository metadata | `/api/{type}/{repo_id}` | models, datasets, spaces | GET, HEAD | Supported | Hub response relayed; limited to 2 MiB |
| Resolve a revision | `/api/{type}/{repo_id}/revision/{revision}` | models, datasets, spaces | GET, HEAD | Supported | Converts mutable references into immutable commits |
| Private or gated repository access | Same metadata and resolve routes | models | GET, HEAD | Supported | Local repository policy plus configured upstream read token |
| List the repository tree | `/api/{type}/{repo_id}/tree/{revision}[/{path}]` | models, datasets, spaces | GET, HEAD | Supported | Allowlisted pagination, recursion, expansion, and metadata parameters |
| List commits | `/api/{type}/{repo_id}/commits/{revision}` | models, datasets, spaces | GET, HEAD | Supported | Non-allowlisted parameters are removed |
| Client identity | `/api/whoami-v2` | n/a | GET, HEAD | Supported | Local hfproxy identity; authentication required |
| Hub health | `/api/health` | n/a | GET, HEAD | Supported | Local `{"status":"ok"}` response |

## Repository types and operations

| Type | File reads | Metadata | Hub writes | Inference / execution |
| --- | --- | --- | --- | --- |
| `models` | Yes | Yes | No | No |
| `datasets` | Yes | Yes | No | No |
| `spaces` | Yes | Yes | No | No |
| Other types | No | No | No | No |

The untyped URL form is accepted only for model downloads, matching the form
used by `huggingface_hub` for models: `/{repo_id}/resolve/...`.

## Security and consistency guarantees

- Every read checks repository policy before serving a cache hit.
- Anonymous reads work only for a repository registered with the `public`
  policy and when the corresponding option is enabled.
- Blobs are addressed by SHA-256; the LFS digest advertised by the Hub is
  verified before publication to S3.
- Interrupted downloads retain only complete 64 MiB blocks in S3; a retry
  resumes from the last committed block and revalidates the upstream ETag and
  `Content-Range` before appending data.
- Mutable references are resolved and retained for `CACHE_REF_TTL`; a valid
  commit is treated as immutable.
- CDN redirects are restricted to allowed HF hosts and query parameters are
  filtered per route.
- Administration operations are separated under `/api/v1` and protected by
  session/bearer token, role, and CSRF checks as applicable.

## Verified out of scope

Uploads and repository write operations, native Xet/CAS, inference, Spaces
execution, multipart ranges, Prometheus metrics, distributed rate limiting, and
paginated cache purge are not implemented. This list must be updated with a
test and code evidence before support is announced.

## Coverage and missing tests

Existing coverage verifies route parsing, nested paths, encoded revisions,
authentication, ETags, ranges, LFS validation, resumable downloads,
singleflight, blob GC, and several concurrency races.

The repository contains protocol-level tests for the request sequence used by
`snapshot_download` and `from_pretrained`. A live matrix against installed
Python client versions and representative public, private, and gated models is
still recommended before declaring complete client-version compatibility.

Remaining audit limitations:

- no automated matrix against multiple real `huggingface_hub` client versions;
- no dedicated integration test for every repository type × metadata endpoint
  combination;
- PostgreSQL/S3 integration tests require external infrastructure and are
  skipped without `HFPROXY_TEST_DATABASE_URL`;
- exact Hub responses may evolve; local tests verify the proxy contract, not
  the contents of every Hub schema.

