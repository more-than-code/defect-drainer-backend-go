package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/joe/defect-drainer-go/internal/env"
)

// Dialect is the inventory database the process opened.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// postgresPingTimeout bounds Ping and the read-only defects probe.
// Long enough for the userspace Tailscale relay, short enough to fail startup.
const postgresPingTimeout = 10 * time.Second

// ParseDBDriver maps DEFECT_DRAINER_DB_DRIVER. Empty and "sqlite" are SQLite.
// Comparison is case-insensitive. Anything else is a startup error.
func ParseDBDriver(raw string) (Dialect, error) {
	v := strings.TrimSpace(raw)
	switch strings.ToLower(v) {
	case "", "sqlite":
		return DialectSQLite, nil
	case "postgres":
		return DialectPostgres, nil
	default:
		return "", fmt.Errorf("DEFECT_DRAINER_DB_DRIVER must be sqlite or postgres, got %q", v)
	}
}

// OpenConfigured opens SQLite via Open, or Postgres when the driver env says so.
// Postgres mode does not create defect-drainer.db and runs no DDL.
func OpenConfigured(dataRoot string) (*sql.DB, Dialect, error) {
	d, err := ParseDBDriver(env.EnvDrainer("DB_DRIVER"))
	if err != nil {
		return nil, "", err
	}
	if d == DialectPostgres {
		sqlDB, err := openPostgres()
		if err != nil {
			return nil, DialectPostgres, err
		}
		return sqlDB, DialectPostgres, nil
	}
	sqlDB, err := Open(dataRoot)
	if err != nil {
		return nil, DialectSQLite, err
	}
	return sqlDB, DialectSQLite, nil
}

// IsPostgres reports whether sqlDB was opened with the Postgres driver.
// It checks the driver type, so there is no package-level mode flag.
func IsPostgres(sqlDB *sql.DB) bool {
	if sqlDB == nil {
		return false
	}
	_, ok := sqlDB.Driver().(*qmarkDriver)
	return ok
}

// openPostgres connects with pgx and checks that defects already exists.
//
// DEFECT_DRAINER_DATABASE_URL is passed through as the conn string. When it
// is empty, the stdlib driver calls pgx.ParseConfig(""). That applies libpq
// defaults (port 5432, sslmode prefer, host the first existing directory
// among /var/run/postgresql, /private/tmp, and /tmp, otherwise localhost)
// and then PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE, and PGSSLMODE.
// A non-empty URL or keyword DSN wins over those variables. If the password
// is still empty, pgx reads its passfile (default ~/.pgpass) itself.
// Ping is the readiness check. No PRAGMA, SCHEMA, ALTER, or FTS runs here.
func openPostgres() (*sql.DB, error) {
	dsn := strings.TrimSpace(env.EnvDrainer("DATABASE_URL"))
	sqlDB, err := sql.Open(qmarkDriverName, dsn)
	if err != nil {
		return nil, postgresErr("open", err, dsn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresPingTimeout)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, postgresErr("ping", err, dsn)
	}
	// A separate timeout so a slow handshake does not expire the probe.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), postgresPingTimeout)
	defer probeCancel()
	rows, err := sqlDB.QueryContext(probeCtx, `SELECT 1 FROM defects LIMIT 1`)
	if err != nil {
		sqlDB.Close()
		return nil, postgresErr("defects table is not readable (schema must already exist; this process does not migrate Postgres)", err, dsn)
	}
	if cerr := rows.Close(); cerr != nil {
		sqlDB.Close()
		return nil, postgresErr("defects table", cerr, dsn)
	}
	return sqlDB, nil
}

func postgresErr(what string, err error, dsn string) error {
	return fmt.Errorf("postgres: %s: %s", what, scrubText(err.Error(), dsn))
}
