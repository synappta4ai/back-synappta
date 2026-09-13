package main

// One-shot dev helper: drops all tenant schemas and the public schema so the
// next boot recreates everything from scratch (fresh migrations + seeds).
import (
	"context"
	"fmt"
	"log"
	"os"

	_ "github.com/joho/godotenv/autoload"

	"synapta/internal/db"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL not set")
	}

	pool, err := db.Open(dsn, db.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()

	// 1. Drop every tenant_* schema.
	rows, err := pool.QueryContext(ctx,
		`SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'tenant_%'`)
	if err != nil {
		log.Fatalf("list schemas: %v", err)
	}
	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			log.Fatalf("scan schema: %v", err)
		}
		schemas = append(schemas, s)
	}
	rows.Close()

	for _, s := range schemas {
		if _, err := pool.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, s)); err != nil {
			log.Fatalf("drop %s: %v", s, err)
		}
		fmt.Printf("dropped schema %s\n", s)
	}

	// 2. Drop public (system tables + goose versioning) and recreate empty.
	if _, err := pool.ExecContext(ctx, `DROP SCHEMA public CASCADE`); err != nil {
		log.Fatalf("drop public: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `CREATE SCHEMA public`); err != nil {
		log.Fatalf("create public: %v", err)
	}
	fmt.Println("dropped + recreated schema public")

	fmt.Println("database reset complete — start the server to re-run migrations")
}
