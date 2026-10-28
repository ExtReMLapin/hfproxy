CREATE TABLE IF NOT EXISTS login_attempts (
    key TEXT PRIMARY KEY,
    window_start TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS login_attempts_window_idx ON login_attempts(window_start);
