#!/usr/bin/env python3
"""Publish and read models and datasets through the real huggingface_hub client.

The proxy has to work with the Hub's own client library, not with an imitation of
it, so this script drives a running proxy with huggingface_hub the way a user
would: create repositories, upload small and large files, read them back, use
branches and tags, change visibility, and check who may do what.

Environment:
  HF_ENDPOINT        address of the proxy, e.g. http://127.0.0.1:8080
  TOKEN_ALICE        publish token of a user named alice (an administrator is fine)
  TOKEN_BOB          publish token of a non-administrator bob, member of the group "research"
  TOKEN_CAROL        publish token of a non-administrator carol, member of "research"
  TOKEN_DAVE         publish token of a non-administrator dave, in no group
  TOKEN_DAVE_READ    read-only token of dave

Usage: python3 scripts/hub-client-test.py
"""

import hashlib
import os
import sys
import tempfile
import traceback

import requests

from huggingface_hub import HfApi, hf_hub_download, snapshot_download
from huggingface_hub.errors import (
    EntryNotFoundError,
    HfHubHTTPError,
    LocalEntryNotFoundError,
    RepositoryNotFoundError,
    RevisionNotFoundError,
)

ENDPOINT = os.environ["HF_ENDPOINT"].rstrip("/")
TOKENS = {name: os.environ["TOKEN_" + name.upper()] for name in ("alice", "bob", "carol", "dave")}
TOKENS["dave-read"] = os.environ["TOKEN_DAVE_READ"]
FAILED = []


def api(who):
    return HfApi(endpoint=ENDPOINT, token=TOKENS[who])


def check(name, condition, detail=""):
    if condition:
        print("ok   ", name)
    else:
        print("FAIL ", name, detail)
        FAILED.append(name)


def expect_error(name, error_type, function, *args, status=None, **kwargs):
    try:
        function(*args, **kwargs)
    except error_type as error:
        got = getattr(getattr(error, "response", None), "status_code", None)
        check(name, status is None or got == status, "status %s, want %s" % (got, status))
    except Exception as error:  # noqa: BLE001
        check(name, False, "raised %s: %s" % (type(error).__name__, error))
    else:
        check(name, False, "did not raise")


def sha256_of(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main():
    work = tempfile.mkdtemp(prefix="hubtest-")
    cache = os.path.join(work, "cache")

    alice = api("alice")
    who = alice.whoami()
    check("whoami reports the user", who["name"] == "alice" and who["auth"]["accessToken"]["role"] == "write", who)
    check("whoami lists the groups of a member", [o["name"] for o in api("bob").whoami()["orgs"]] == ["research"])
    check("a read token is reported as read", api("dave-read").whoami()["auth"]["accessToken"]["role"] == "read")

    # --- a model, created and filled the way people do it ---
    repo = "alice/tiny-model"
    url = alice.create_repo(repo, repo_type="model", exist_ok=False)
    check("create_repo returns the repository address", str(url).endswith("/alice/tiny-model"), url)
    check("repo_exists", alice.repo_exists(repo))
    expect_error("creating it twice conflicts", HfHubHTTPError, alice.create_repo, repo, status=409)
    alice.create_repo(repo, exist_ok=True)

    small = os.path.join(work, "config.json")
    with open(small, "w") as handle:
        handle.write('{"model_type": "tiny", "layers": 2}\n')
    commit = alice.upload_file(path_or_fileobj=small, path_in_repo="config.json", repo_id=repo, commit_message="add config")
    check("upload_file of a small file", commit.oid and len(commit.oid) == 40, commit)

    weights = os.path.join(work, "model.safetensors")
    with open(weights, "wb") as handle:
        handle.write(os.urandom(3 << 20))
        handle.write(b"\0" * (1 << 20))
    big = os.path.join(work, "big.bin")
    with open(big, "wb") as handle:
        for _ in range(12):  # 12 MiB: above the 10 MiB limit for plain Git files
            handle.write(os.urandom(1 << 20))
    folder = os.path.join(work, "folder")
    os.makedirs(os.path.join(folder, "sub", "deeper"))
    for rel, text in {"README.md": "# Tiny\nA test model.\n", "sub/tokenizer.json": "{}\n", "sub/deeper/notes.txt": "hello\n"}.items():
        with open(os.path.join(folder, rel), "w") as handle:
            handle.write(text)
    import shutil

    shutil.copy(weights, os.path.join(folder, "model.safetensors"))
    shutil.copy(big, os.path.join(folder, "sub", "big.bin"))
    info = alice.upload_folder(folder_path=folder, repo_id=repo, commit_message="add the rest")
    check("upload_folder with LFS files", info.oid and len(info.oid) == 40, info)

    files = sorted(alice.list_repo_files(repo))
    expected = ["README.md", "config.json", "model.safetensors", "sub/big.bin", "sub/deeper/notes.txt", "sub/tokenizer.json"]
    check("list_repo_files", files == expected, files)

    # --- reading it back ---
    path = hf_hub_download(repo, "config.json", endpoint=ENDPOINT, token=TOKENS["alice"], cache_dir=cache)
    check("hf_hub_download of a regular file", open(path).read() == open(small).read())
    path = hf_hub_download(repo, "model.safetensors", endpoint=ENDPOINT, token=TOKENS["alice"], cache_dir=cache)
    check("hf_hub_download of an LFS file", sha256_of(path) == sha256_of(weights))
    check("the blob of an LFS file is named by its SHA-256", os.path.basename(os.path.realpath(path)) == sha256_of(weights))
    snap = snapshot_download(repo, endpoint=ENDPOINT, token=TOKENS["alice"], cache_dir=cache)
    check("snapshot_download", sha256_of(os.path.join(snap, "sub", "big.bin")) == sha256_of(big) and os.path.exists(os.path.join(snap, "sub/deeper/notes.txt")))
    check("file_exists", alice.file_exists(repo, "sub/tokenizer.json") and not alice.file_exists(repo, "nothing.txt"))
    expect_error("a missing file", EntryNotFoundError, hf_hub_download, repo, "nothing.txt", endpoint=ENDPOINT, token=TOKENS["alice"], cache_dir=cache)
    expect_error("a missing revision", RevisionNotFoundError, hf_hub_download, repo, "config.json", revision="nope", endpoint=ENDPOINT, token=TOKENS["alice"], cache_dir=cache)

    model = alice.model_info(repo)
    check("model_info lists the files with sizes", sorted(s.rfilename for s in model.siblings) == expected and model.sha == info.oid, model)
    check("model_info reports LFS files", {s.rfilename: s.lfs is not None for s in model.siblings}["model.safetensors"])
    tree = list(alice.list_repo_tree(repo, recursive=True))
    check("list_repo_tree", sorted(t.path for t in tree if hasattr(t, "size")) == expected and any(t.path == "sub/deeper" for t in tree), [t.path for t in tree])
    top = {t.path for t in alice.list_repo_tree(repo)}
    check("list_repo_tree of the top level", top == {"README.md", "config.json", "model.safetensors", "sub"}, top)
    paths = alice.get_paths_info(repo, ["config.json", "sub", "ghost"])
    check("get_paths_info", sorted(p.path for p in paths) == ["config.json", "sub"], paths)

    # --- history, branches and tags ---
    commits = alice.list_repo_commits(repo)
    check("list_repo_commits", [c.title for c in commits][:2] == ["add the rest", "add config"], [c.title for c in commits])
    alice.create_tag(repo, tag="v1", revision=commit.oid)
    alice.create_branch(repo, branch="dev")
    refs = alice.list_repo_refs(repo)
    check("list_repo_refs", {b.name for b in refs.branches} == {"main", "dev"} and [t.name for t in refs.tags] == ["v1"], refs)
    old = alice.list_repo_files(repo, revision="v1")
    check("a tag shows the repository as it was", old == ["config.json"], old)
    alice.upload_file(path_or_fileobj=b"on the dev branch\n", path_in_repo="dev.txt", repo_id=repo, revision="dev", commit_message="dev only")
    check("a branch has its own history", "dev.txt" in alice.list_repo_files(repo, revision="dev") and "dev.txt" not in alice.list_repo_files(repo))
    alice.delete_file("config.json", repo, commit_message="drop config")
    check("delete_file", "config.json" not in alice.list_repo_files(repo) and "config.json" in alice.list_repo_files(repo, revision="v1"))
    alice.delete_folder("sub", repo, commit_message="drop sub")
    check("delete_folder", not any(f.startswith("sub/") for f in alice.list_repo_files(repo)))
    alice.delete_tag(repo, tag="v1")
    alice.delete_branch(repo, branch="dev")
    check("deleting a tag and a branch", not alice.list_repo_refs(repo).tags and len(alice.list_repo_refs(repo).branches) == 1)
    before = len(alice.list_repo_commits(repo))
    alice.upload_file(path_or_fileobj=weights, path_in_repo="model.safetensors", repo_id=repo, commit_message="same weights")
    check("uploading identical content makes no new commit", len(alice.list_repo_commits(repo)) == before)

    # --- what the download endpoint tells clients ---
    auth = {"Authorization": "Bearer " + TOKENS["alice"]}
    head = requests.head(ENDPOINT + "/" + repo + "/resolve/main/model.safetensors", headers=auth, allow_redirects=False)
    check("HEAD of an LFS file", head.status_code == 200 and head.headers["X-Linked-Etag"].strip('"') == sha256_of(weights) and head.headers["X-Linked-Size"] == str(os.path.getsize(weights)) and len(head.headers["X-Repo-Commit"]) == 40, dict(head.headers))
    head = requests.head(ENDPOINT + "/" + repo + "/resolve/main/README.md", headers=auth)
    check("HEAD of a regular file carries its Git object name", len(head.headers["ETag"].strip('"')) == 40 and "X-Linked-Etag" not in head.headers, dict(head.headers))
    again = requests.get(ENDPOINT + "/" + repo + "/resolve/main/README.md", headers={**auth, "If-None-Match": head.headers["ETag"]})
    check("a conditional request is answered 304", again.status_code == 304, again.status_code)
    part = requests.get(ENDPOINT + "/" + repo + "/resolve/main/model.safetensors", headers={**auth, "Range": "bytes=10-19"})
    with open(weights, "rb") as handle:
        handle.seek(10)
        want = handle.read(10)
    check("a range request", part.status_code == 206 and part.content == want and part.headers["Content-Range"].startswith("bytes 10-19/"), part.status_code)
    by_commit = requests.get(ENDPOINT + "/" + repo + "/resolve/" + head.headers["X-Repo-Commit"] + "/README.md", headers=auth)
    check("a file by commit hash", by_commit.status_code == 200 and by_commit.text.startswith("# Tiny"))

    # --- the upload protocol itself ---
    body = b"x" * 1000
    oid = hashlib.sha256(body).hexdigest()
    batch_url = ENDPOINT + "/" + repo + ".git/info/lfs/objects/batch"
    batch = requests.post(batch_url, json={"operation": "upload", "transfers": ["basic"], "objects": [{"oid": oid, "size": len(body)}]}, headers=auth)
    action = batch.json()["objects"][0]["actions"]["upload"]["href"]
    wrong = requests.put(action, data=b"y" * 1000)
    check("content that does not match its hash is refused", wrong.status_code == 422, wrong.status_code)
    short = requests.put(action, data=b"x" * 999)
    check("content of the wrong size is refused", short.status_code == 400, short.status_code)
    tampered = requests.put(action.replace("size=1000", "size=1001"), data=b"x" * 1001)
    check("a tampered upload link is refused", tampered.status_code == 403, tampered.status_code)
    nobody = requests.post(batch_url, json={"operation": "upload", "objects": [{"oid": oid, "size": len(body)}]})
    check("the LFS batch needs a token", nobody.status_code == 401, nobody.status_code)
    stranger = requests.post(batch_url, json={"operation": "upload", "objects": [{"oid": oid, "size": len(body)}]}, headers={"Authorization": "Bearer " + TOKENS["bob"]})
    check("and rights on the namespace", stranger.status_code == 403, stranger.status_code)
    ok_put = requests.put(action, data=body)
    check("a correct upload", ok_put.status_code == 200, ok_put.status_code)
    again = requests.post(batch_url, json={"operation": "upload", "objects": [{"oid": oid, "size": len(body)}]}, headers=auth)
    check("an object already stored needs no upload", "actions" not in again.json()["objects"][0])
    commit_url = ENDPOINT + "/api/models/" + repo + "/commit/main"
    missing = requests.post(commit_url, data='{"key":"header","value":{"summary":"x"}}\n{"key":"lfsFile","value":{"path":"ghost.bin","algo":"sha256","oid":"' + "0" * 64 + '","size":5}}\n', headers=auth)
    check("a commit naming content that was never uploaded", missing.status_code == 400, missing.text)
    no_summary = requests.post(commit_url, data='{"key":"header","value":{"summary":""}}\n', headers=auth)
    check("a commit without a summary", no_summary.status_code == 400, no_summary.status_code)
    stale = requests.post(commit_url, data='{"key":"header","value":{"summary":"late","parentCommit":"' + "1" * 40 + '"}}\n', headers=auth)
    check("a commit on a branch that moved", stale.status_code == 412, stale.status_code)
    pr = requests.post(commit_url + "?create_pr=1", data='{"key":"header","value":{"summary":"pr"}}\n', headers=auth)
    check("pull requests are refused", pr.status_code == 400, pr.status_code)
    evil = requests.post(commit_url, data='{"key":"header","value":{"summary":"x"}}\n{"key":"file","value":{"path":"../escape.txt","content":"eA==","encoding":"base64"}}\n', headers=auth)
    check("a path that leaves the repository", evil.status_code == 400, evil.status_code)
    git_dir = requests.post(commit_url, data='{"key":"header","value":{"summary":"x"}}\n{"key":"file","value":{"path":"a/.git/config","content":"eA==","encoding":"base64"}}\n', headers=auth)
    check("a .git directory", git_dir.status_code == 400, git_dir.status_code)

    # --- who may do what ---
    expect_error("another user cannot write to my namespace", HfHubHTTPError, api("bob").upload_file, path_or_fileobj=b"x", path_in_repo="x.txt", repo_id=repo, status=403)
    expect_error("nor create a repository there", HfHubHTTPError, api("bob").create_repo, "alice/other", status=403)
    expect_error("a read-only token cannot publish", HfHubHTTPError, api("dave-read").create_repo, "dave/x", status=403)
    api("dave").create_repo("dave/own-model")
    check("a user publishes under their own name", api("dave").repo_exists("dave/own-model"))
    expect_error("a namespace that does not exist", HfHubHTTPError, alice.create_repo, "nobody-here/model", status=403)
    expect_error("a reserved namespace", HfHubHTTPError, alice.create_repo, "datasets/model", status=403)
    refused = requests.post(ENDPOINT + "/api/repos/create", json={"name": "-bad--name", "organization": "alice"}, headers={"Authorization": "Bearer " + TOKENS["alice"]})
    check("a name the Hub would refuse", refused.status_code == 400, refused.text)
    refused = requests.post(ENDPOINT + "/api/repos/create", json={"name": "model.git", "organization": "alice"}, headers={"Authorization": "Bearer " + TOKENS["alice"]})
    check("a name ending in .git", refused.status_code == 400, refused.text)
    session_only = requests.post(ENDPOINT + "/api/repos/create", json={"name": "nope", "organization": "alice"})
    check("publishing without a token", session_only.status_code == 401, session_only.status_code)

    # --- private repositories and groups ---
    api("bob").create_repo("research/shared-model", private=True)
    check("a group member publishes under the group", api("carol").repo_exists("research/shared-model"))
    api("carol").upload_file(path_or_fileobj=b"group file\n", path_in_repo="notes.txt", repo_id="research/shared-model")
    check("another member writes to it too", "notes.txt" in api("bob").list_repo_files("research/shared-model"))
    expect_error("a private repository is hidden from others", RepositoryNotFoundError, api("dave").model_info, "research/shared-model")
    expect_error("and cannot be read through resolve", (RepositoryNotFoundError, HfHubHTTPError, LocalEntryNotFoundError), hf_hub_download, "research/shared-model", "notes.txt", endpoint=ENDPOINT, token=TOKENS["dave"], cache_dir=cache)
    check("an administrator can read it", api("alice").model_info("research/shared-model").private is True)
    api("bob").update_repo_settings("research/shared-model", private=False)
    check("making it public", api("dave").model_info("research/shared-model").private is False)
    path = hf_hub_download("research/shared-model", "notes.txt", endpoint=ENDPOINT, token=TOKENS["dave"], cache_dir=cache)
    check("then everyone reads it", open(path).read() == "group file\n")
    api("bob").update_repo_settings("research/shared-model", private=True)
    expect_error("and private again", RepositoryNotFoundError, api("dave").model_info, "research/shared-model")

    # --- a dataset ---
    data = "alice/tiny-data"
    alice.create_repo(data, repo_type="dataset")
    alice.upload_file(path_or_fileobj=b"a,b\n1,2\n3,4\n", path_in_repo="data/train.csv", repo_id=data, repo_type="dataset", commit_message="train")
    parquet = os.path.join(work, "part.parquet")
    with open(parquet, "wb") as handle:
        handle.write(os.urandom(2 << 20))
    alice.upload_file(path_or_fileobj=parquet, path_in_repo="data/part.parquet", repo_id=data, repo_type="dataset")
    check("list dataset files", sorted(alice.list_repo_files(data, repo_type="dataset")) == ["data/part.parquet", "data/train.csv"])
    path = hf_hub_download(data, "data/train.csv", repo_type="dataset", endpoint=ENDPOINT, token=TOKENS["alice"], cache_dir=cache)
    check("hf_hub_download of a dataset file", open(path).read() == "a,b\n1,2\n3,4\n")
    snap = snapshot_download(data, repo_type="dataset", endpoint=ENDPOINT, token=TOKENS["alice"], cache_dir=cache)
    check("snapshot_download of a dataset", sha256_of(os.path.join(snap, "data", "part.parquet")) == sha256_of(parquet))
    check("dataset_info", alice.dataset_info(data).id == data)
    alice.delete_repo(data, repo_type="dataset")
    check("delete_repo", not alice.repo_exists(data, repo_type="dataset"))

    # --- the same name can be reused after deleting ---
    alice.delete_repo(repo)
    check("a deleted repository is gone", not alice.repo_exists(repo))
    alice.create_repo(repo)
    check("and its name can be used again", alice.list_repo_files(repo) == [])
    alice.delete_repo(repo)
    api("bob").delete_repo("research/shared-model")
    api("dave").delete_repo("dave/own-model")

    print()
    if FAILED:
        print("%d check(s) failed: %s" % (len(FAILED), ", ".join(FAILED)))
        sys.exit(1)
    print("all checks passed")


if __name__ == "__main__":
    try:
        main()
    except Exception:  # noqa: BLE001
        traceback.print_exc()
        sys.exit(2)
