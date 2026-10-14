// Command migrate applies or rolls back todo-service database migrations.
package main

import (
	"database/sql"
	"log"
	"os"
	"strconv"

	_ "github.com/lib/pq"
	"todo-service/pgstore"
)

func main() {
	if (len(os.Args) != 2 || (os.Args[1] != "up" && os.Args[1] != "down")) &&
		!(len(os.Args) == 3 && os.Args[1] == "force") {
		log.Fatal("usage: migrate up|down|force VERSION")
	}
	dsn := os.Getenv("TODO_DATABASE_URL")
	if dsn == "" {
		log.Fatal("TODO_DATABASE_URL is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	var migrateErr error
	switch os.Args[1] {
	case "up":
		migrateErr = pgstore.Migrate(db)
	case "down":
		migrateErr = pgstore.MigrateDown(db)
	case "force":
		version, err := strconv.Atoi(os.Args[2])
		if err != nil {
			log.Fatalf("invalid migration version %q: %v", os.Args[2], err)
		}
		migrateErr = pgstore.MigrateForce(db, version)
	}
	if migrateErr != nil {
		log.Fatal(migrateErr)
	}
}
