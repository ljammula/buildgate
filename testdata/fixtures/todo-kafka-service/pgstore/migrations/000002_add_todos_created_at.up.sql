-- Existing rows get the migration's own timestamp: their real creation time
-- was never recorded, and a NOT NULL column needs some value.
ALTER TABLE todos ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS todos_created_at_id_idx ON todos (created_at, id);
