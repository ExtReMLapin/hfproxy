-- hfproxy never writes to a repository, so the only permission a grant can give
-- is to read it. "write" and "admin" were accepted but enforced as "read".
UPDATE repo_grants SET permission='read' WHERE permission<>'read';
ALTER TABLE repo_grants DROP CONSTRAINT IF EXISTS repo_grants_permission_check;
ALTER TABLE repo_grants ADD CONSTRAINT repo_grants_permission_check CHECK (permission='read');
