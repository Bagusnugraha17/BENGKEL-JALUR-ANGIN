package database

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"koplink/migrations"
)

func Migrate(ctx context.Context, p *pgxpool.Pool) error {
	tx, e := p.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(791231); CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY)`); e != nil {
		return e
	}
	for _, migration := range []struct{ version, filename string }{
		{"001", "001_init.sql"},
		{"002", "002_registration_details.sql"},
		{"003", "003_incremental_fintech_modules.sql"},
		{"004", "004_role_workspaces.sql"},
	} {
		var applied int
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE version=$1", migration.version).Scan(&applied); e != nil {
			return e
		}
		if applied > 0 {
			continue
		}
		b, readErr := migrations.Files.ReadFile(migration.filename)
		if readErr != nil {
			return fmt.Errorf("read migration %s: %w", migration.filename, readErr)
		}
		if _, e = tx.Exec(ctx, string(b)); e != nil {
			return fmt.Errorf("migration %s: %w", migration.version, e)
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", migration.version); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
