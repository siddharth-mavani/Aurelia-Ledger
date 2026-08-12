package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// migrationLockID serializes schema changes made by this application per database.
const migrationLockID int64 = 721_942_583_101

type Migration struct {
	Version string
	SQL     string
}

// LoadMigrations returns regular .sql files in lexicographic filename order.
func LoadMigrations(directory string) ([]Migration, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}

	filenames := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		filenames = append(filenames, entry.Name())
	}
	sort.Strings(filenames)
	if len(filenames) == 0 {
		return nil, fmt.Errorf("no .sql migration files found in %q", directory)
	}

	migrations := make([]Migration, 0, len(filenames))
	for _, filename := range filenames {
		contents, err := os.ReadFile(filepath.Join(directory, filename))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", filename, err)
		}
		migrations = append(migrations, Migration{Version: filename, SQL: string(contents)})
	}
	return migrations, nil
}

// ApplyMigrations applies each unapplied migration and records it atomically.
// A PostgreSQL advisory lock prevents concurrent migrate processes from racing.
func ApplyMigrations(ctx context.Context, db *sql.DB, migrations []Migration) error {
	if len(migrations) == 0 {
		return fmt.Errorf("no migrations supplied")
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Advisory locks belong to this connection, so always unlock before it returns to the pool.
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, migration := range migrations {
		var applied bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, migration.Version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %q: %w", migration.Version, err)
		}
		if applied {
			continue
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %q: %w", migration.Version, err)
		}
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %q: %w", migration.Version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, migration.Version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %q: %w", migration.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %q: %w", migration.Version, err)
		}
	}
	return nil
}
