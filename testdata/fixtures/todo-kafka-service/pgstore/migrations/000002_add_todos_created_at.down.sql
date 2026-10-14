DROP INDEX IF EXISTS todos_created_at_id_idx;
ALTER TABLE todos DROP COLUMN IF EXISTS created_at;
