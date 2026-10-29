# Publishing models and datasets

hfproxy can also be the place where your own models and datasets live. A user publishes them on the proxy, under their name or a group's, with the Hugging Face tools they already use (`huggingface_hub`, the `hf` command, `transformers`, `datasets`) or from the console. **Nothing is sent to Hugging Face**: the files go to your object storage, the catalog to PostgreSQL.

Published repositories are served through the same HTTP API as the Hub, so a client only needs `HF_ENDPOINT` to point at the proxy.

## Quick start

1. Open **Access tokens** in the console and create a token that can **publish** (a read-only token cannot create or change anything).
2. Point the tools at the proxy and use the token:

```bash
export HF_ENDPOINT=https://hfproxy.example.com
export HF_TOKEN=hfpx_...

hf repo create my-model                       # alice/my-model, under your own name
hf upload alice/my-model ./model-folder .     # small files inside commits, large ones through Git LFS
hf download alice/my-model
```

```python
from huggingface_hub import HfApi, snapshot_download

api = HfApi(endpoint="https://hfproxy.example.com", token="hfpx_...")
api.create_repo("research/shared-corpus", repo_type="dataset", private=True)
api.upload_folder(folder_path="./corpus", repo_id="research/shared-corpus", repo_type="dataset")
snapshot_download("research/shared-corpus", repo_type="dataset")
```

The console's **Models & datasets** page does the same without a command line: create a repository, add files or a whole folder (with a progress bar), browse the files, read the model card, see the history, make a repository private or shared, delete it.

## Who can publish where

A repository is named `namespace/name`. The namespace is the **name of a user or of a group**:

- a user publishes under their own name;
- the members of a group publish under the group's name (any member can add to, change and delete what the group owns);
- an administrator can publish under any existing name;
- a name that is neither a user nor a group (`nobody/model`) is refused, and so are a few reserved names (`api`, `models`, `datasets`, `spaces`, `auth`, `assets`, `login`, `logout`, `healthz`, `readyz`, `metrics`, `static`). A user and a group can no longer share a name, because the name would not tell whose it is.

Names follow the Hub's rules: letters, digits, `-`, `_` and `.`, at most 96 characters, no leading or trailing `-` or `.`, no `--` or `..`, and not ending in `.git`. A name must also be usable in a URL, so a user whose name has other characters (an e-mail address, say) cannot publish under it.

A **token needs the `publish` scope** to create or change anything. Publishing is only possible with a bearer token, never with the browser session: the console has its own routes for that, protected against cross-site requests. An administrator creates such a token for a user with `hfproxy admin token create --username NAME --scope publish`.

## Visibility

A repository is **shared** (public, in the Hub's words) or **private**:

- shared: any signed-in user of the proxy can read it (anonymous visitors too, only if anonymous read of public repositories is turned on);
- private: only the owner's namespace can read it, which means the user themselves or the members of the group, plus administrators.

Visibility can be changed later from the console or with `update_repo_settings(private=...)`.

## What is supported

The routes the Hub's clients call to publish and read are implemented: `whoami-v2` (the groups of a user are the "organizations" the client lists), create and delete a repository, repository settings (visibility), `preupload`, the Git LFS batch / upload / verify endpoints, the commit API (files, large files, deletions, folder deletions, copies), branches and tags (create and delete), `revision`, `tree` (recursive or not), `paths-info`, `refs`, `commits`, `validate-yaml`, and `resolve` for downloads with `Range`, `ETag` / `If-None-Match`, and the `X-Repo-Commit`, `X-Linked-Etag` and `X-Linked-Size` headers the clients read.

History is real: every commit has a name (40 hexadecimal characters, like Git's), a parent, an author and a message, and a branch or tag can be read at any time. Commits go to a **branch**; a tag or a commit is read-only. `main` exists from the start.

The README.md at the root is the model card. Its YAML header (license, tags, pipeline, language...) is checked when a client uploads it, and read back as the repository's tags and card data.

### What is not supported

- **Xet storage**: the proxy never announces it, so clients fall back to Git LFS. Files are sent through LFS's `basic` transfer (one request per file), not multipart: a file as big as the limit goes in one request.
  `huggingface_hub` 1.x and later uses Xet by default when `hf_xet` is installed, and the proxy answers 404 to its token request. Uploading from such a client needs `HF_HUB_DISABLE_XET=1` (or `pip uninstall hf_xet`); downloads are not affected.
- **Git itself** (`git clone`, `git push`, `git lfs`): only the HTTP API. To get a copy of a repository, use `snapshot_download` or `hf download`.
- **Pull requests** (`create_pr`), discussions, likes, gated access, renaming or moving a repository (`move_repo`), squashing history, Spaces, inference.
- **Download statistics on a model** (the "downloads" counter of the Hub) are not kept per repository, and tags are only those of the card.

## Limits

All of them are settings of the **Publishing** section of the console (or `HFPROXY_*` variables):

| Setting | Variable | Default | |
| --- | --- | --- | --- |
| Publishing on or off | `HFPROXY_CUSTOM_REPOSITORIES` | on | When off, nothing can be created or changed; what exists stays readable. |
| Largest file | `HFPROXY_UPLOAD_MAX_FILE_SIZE` | 50 GiB | At most 625 GiB: the object store accepts 10 000 parts of 64 MiB per file. |
| Storage quota per user | `HFPROXY_UPLOAD_USER_QUOTA` | 0 (none) | Counted over the repositories a user created, each distinct file once, history included. |

With the [safe-formats setting](safe-formats.md) on, a file that can run code when loaded (`.bin`, `.pt`, `.pth`, `.ckpt`, `.pkl`...) is refused with a 422 and a file named `.safetensors` must have a valid safetensors header.

Files bigger than 10 MiB, and the usual weight formats (`.safetensors`, `.bin`, `.gguf`, `.parquet`, `.h5`, archives...), travel through Git LFS; the others are sent inside the commit (at most 32 MiB each). A commit request is capped at 512 MiB.

## How it is stored

- **Content** lives in the same object storage and the same `blobs` table as the cache, deduplicated by SHA-256: the same file in two repositories, or in two commits, is stored once.
- **A commit stores what it changed**, not a copy of the tree, and every 32 commits a snapshot of the whole tree. A dataset of a hundred thousand files uploaded 50 at a time does not produce a hundred million rows; reading a tree walks at most 32 commits.
- **Published content is never evicted.** The cache size limit (`HFPROXY_CACHE_MAX_TOTAL_SIZE`) and the least-recently-used eviction only concern files cached from the Hub, and published bytes do not count against that limit. They count against the user's quota and against your object storage capacity: watch the bucket, not only the cache.
- **Deleting a repository** removes its history and releases its content; the collector deletes what no other repository uses after `HFPROXY_CACHE_GC_GRACE`. Deleting a file with a commit does not free space, because the earlier commits still hold it.
- **An upload that never gets committed** (a client that stopped between the upload and the commit) is kept for 24 hours, then collected.
- A repository cached from the Hub and a published repository cannot have the same name: the console refuses to register a Hub repository over a published one.

## Backups

The catalog (PostgreSQL) and the bucket are both needed to restore published content, exactly like the cache, but with one difference: **a cached file can be fetched again from the Hub, a published one cannot.** Back both up, and test the restore ([Backups](operations.md#backup-and-restore)). If an object goes missing from the bucket, reads of that file answer `502` and the log says `the content of a published file is missing from object storage`; `hfproxy cache verify` lists such files.

## Operating it

- Audit log: `repository.created`, `repository.deleted`, `repository.visibility`, `repository.commit`, `repository.branch.*`, `repository.tag.*`, with the person and whether the console or the Hub API did it.
- Metrics: `hfproxy_published_bytes_total` (bytes received for new content), `hfproxy_published_commits_total`. Downloads of published files are counted with the others (`hfproxy_downloads_total`, the downloads table, the download limits and quotas).
- Console: **Repositories** (administrators) shows published repositories next to the cached ones, marked *Published here*; deleting one there removes it with its history.
- The Hub-compatible write routes accept bearer tokens only and ignore cookies.

## Checking compatibility

`scripts/hub-client-test.py` drives a running proxy with the real `huggingface_hub` library: repositories, small and large files, folders, branches and tags, deletion, visibility, groups, permissions, the LFS protocol and its refusals. CI runs it on every change (the `publishing` job), and you can run it against your own instance with test users: see the header of the script.
