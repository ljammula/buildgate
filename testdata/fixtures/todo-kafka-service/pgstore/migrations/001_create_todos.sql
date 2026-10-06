-- IF NOT EXISTS: databases created before migrations existed already have
-- this table (pgstore.New used to create it inline), and must adopt this
-- version without an error.
CREATE TABLE IF NOT EXISTS todos (
	id    TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	done  BOOLEAN NOT NULL DEFAULT false
);
