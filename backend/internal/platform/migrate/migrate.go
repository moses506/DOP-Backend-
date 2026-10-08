// Package migrate applies SQL migrations owned by each module.
//
// Every module ships its own migrations directory (embedded) with files named
// NNNN_description.sql. Versions are tracked per module, so modules can be
// added later without renumbering anything that already exists.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Source struct {
	Module string
	FS     fs.FS // root must contain the .sql files
}

type file struct {
	version int
	name    string
}

// lockKey is an arbitrary constant so only one instance migrates at a time.
const lockKey = 7_400_311

func Run(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, sources []Source) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) //nolint:errcheck

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			module     text        NOT NULL,
			version    int         NOT NULL,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (module, version)
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, src := range sources {
		files, err := list(src.FS)
		if err != nil {
			return fmt.Errorf("%s: %w", src.Module, err)
		}
		for _, f := range files {
			var exists bool
			if err := conn.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE module=$1 AND version=$2)`,
				src.Module, f.version).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			body, err := fs.ReadFile(src.FS, f.name)
			if err != nil {
				return err
			}
			err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, string(body)); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`INSERT INTO schema_migrations (module, version, name) VALUES ($1,$2,$3)`,
					src.Module, f.version, f.name)
				return err
			})
			if err != nil {
				return fmt.Errorf("migration %s/%s: %w", src.Module, f.name, err)
			}
			log.Info("migration applied", "module", src.Module, "file", f.name)
		}
	}
	return nil
}

func list(fsys fs.FS) ([]file, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []file
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q must be named NNNN_description.sql", e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version prefix", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		out = append(out, file{version: v, name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}
