-- Models and datasets published on the proxy itself rather than cached from the
-- Hub. A "local" repository has a history of commits and named references
-- (branches and tags); the files of a commit are rows of artifacts, which already
-- point at deduplicated blobs in object storage.
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'hub' CHECK (origin IN ('hub','local'));
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
-- Whether a file is stored the way the Hub stores large files (Git LFS). It
-- decides the headers and the tree entries clients see.
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS lfs BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS local_commits (
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    sha TEXT NOT NULL,
    parent_sha TEXT NOT NULL DEFAULT '',
    author_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    author_name TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A commit stores only what it changed (artifacts for files added or changed,
    -- local_tombstones for files removed), so that a big dataset uploaded in many
    -- small commits does not copy its whole tree each time. Every so often a
    -- commit is a snapshot: it stores the complete tree and ends the chain that
    -- reading a tree has to follow. depth counts commits since the last snapshot.
    snapshot BOOLEAN NOT NULL DEFAULT true,
    depth INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (repository_id, sha)
);
CREATE INDEX IF NOT EXISTS local_commits_parent_idx ON local_commits(repository_id, parent_sha);

CREATE TABLE IF NOT EXISTS local_tombstones (
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL,
    path TEXT NOT NULL,
    PRIMARY KEY (repository_id, commit_sha, path)
);

CREATE TABLE IF NOT EXISTS local_refs (
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('branch','tag')),
    name TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, kind, name)
);

-- Who published how much: the quota sums the blobs of the repositories a user owns.
CREATE INDEX IF NOT EXISTS repositories_owner_idx ON repositories(owner_user_id) WHERE origin='local';

-- A published repository lives under the name of a user or of a group
-- ("namespace/name"), so a user and a group cannot share a name from now on.
-- (Existing duplicates are left alone; publishing under one is refused.)
CREATE OR REPLACE FUNCTION hfproxy_namespace_user() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM groups WHERE name = NEW.username) THEN
        RAISE EXCEPTION 'a group is already named %', NEW.username USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE OR REPLACE FUNCTION hfproxy_namespace_group() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM users WHERE username = NEW.name) THEN
        RAISE EXCEPTION 'a user is already named %', NEW.name USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS users_namespace ON users;
CREATE TRIGGER users_namespace BEFORE INSERT OR UPDATE OF username ON users
    FOR EACH ROW EXECUTE FUNCTION hfproxy_namespace_user();
DROP TRIGGER IF EXISTS groups_namespace ON groups;
CREATE TRIGGER groups_namespace BEFORE INSERT OR UPDATE OF name ON groups
    FOR EACH ROW EXECUTE FUNCTION hfproxy_namespace_group();
