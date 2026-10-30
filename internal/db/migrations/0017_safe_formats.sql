-- A repository can be exempted from the "safe formats only" setting, so one
-- model that only ships pickle weights can still be served.
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS allow_unsafe_formats boolean NOT NULL DEFAULT false;
