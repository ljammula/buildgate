-- IF NOT EXISTS lets databases created by the earlier startup migration
-- adopt this version without an error.
CREATE TABLE IF NOT EXISTS todos (
	id    TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	done  BOOLEAN NOT NULL DEFAULT false
);
