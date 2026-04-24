// Package db wires Postgres into an *ent.Client. Callers pass the pooled
// *sql.DB around to the migrate package so migrations and runtime share one
// connection pool.
package db

import (
	"database/sql"
	"fmt"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/spacefleet/app/ent"
)

// Open returns a sql.DB connected via pgx's database/sql shim and an
// *ent.Client that wraps it. Callers own both and must Close them.
func Open(dsn string) (*sql.DB, *ent.Client, error) {
	if dsn == "" {
		return nil, nil, fmt.Errorf("DATABASE_URL is empty")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("ping postgres: %w", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	return sqlDB, client, nil
}
