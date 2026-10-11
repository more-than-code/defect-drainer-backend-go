package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
)

func TestRebindDollar(t *testing.T) {
	upsert := `INSERT INTO job_summary (
      job_id, batch_id, app_id, status, mode, defect_count,
      pr_created, pr_merged, pr_open, error, created_at, updated_at, completed_at
    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
    ON CONFLICT(job_id) DO UPDATE SET
      batch_id=excluded.batch_id,
      app_id=excluded.app_id,
      status=excluded.status,
      mode=excluded.mode,
      defect_count=excluded.defect_count,
      pr_created=excluded.pr_created,
      pr_merged=excluded.pr_merged,
      pr_open=excluded.pr_open,
      error=excluded.error,
      updated_at=excluded.updated_at,
      completed_at=COALESCE(job_summary.completed_at, excluded.completed_at)`
	upsertWant := `INSERT INTO job_summary (
      job_id, batch_id, app_id, status, mode, defect_count,
      pr_created, pr_merged, pr_open, error, created_at, updated_at, completed_at
    ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
    ON CONFLICT(job_id) DO UPDATE SET
      batch_id=excluded.batch_id,
      app_id=excluded.app_id,
      status=excluded.status,
      mode=excluded.mode,
      defect_count=excluded.defect_count,
      pr_created=excluded.pr_created,
      pr_merged=excluded.pr_merged,
      pr_open=excluded.pr_open,
      error=excluded.error,
      updated_at=excluded.updated_at,
      completed_at=COALESCE(job_summary.completed_at, excluded.completed_at)`

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "none", in: "SELECT 1", want: "SELECT 1"},
		{name: "one", in: "SELECT * FROM defects WHERE id = ?", want: "SELECT * FROM defects WHERE id = $1"},
		{name: "many", in: "VALUES (?,?,?)", want: "VALUES ($1,$2,$3)"},
		{name: "single quotes", in: "SELECT '?' , ?", want: "SELECT '?' , $1"},
		{name: "escaped single quote", in: "SELECT 'it''s a ?' , ?", want: "SELECT 'it''s a ?' , $1"},
		{name: "double quotes", in: `SELECT "col?" , ?`, want: `SELECT "col?" , $1`},
		{name: "escaped double quote", in: `SELECT "a""b?" , ?`, want: `SELECT "a""b?" , $1`},
		{name: "line comment", in: "SELECT ? -- ?\n, ?", want: "SELECT $1 -- ?\n, $2"},
		{name: "block comment", in: "SELECT ? /* ? */, ?", want: "SELECT $1 /* ? */, $2"},
		{name: "comment inside string", in: "SELECT '-- ?', ?", want: "SELECT '-- ?', $1"},
		{name: "job_summary upsert", in: upsert, want: upsertWant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RebindDollar(tt.in); got != tt.want {
				t.Fatalf("RebindDollar(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseDBDriver(t *testing.T) {
	tests := []struct {
		in      string
		want    Dialect
		wantErr bool
	}{
		{in: "", want: DialectSQLite},
		{in: "   ", want: DialectSQLite},
		{in: "sqlite", want: DialectSQLite},
		{in: "SQLite", want: DialectSQLite},
		{in: "postgres", want: DialectPostgres},
		{in: " Postgres ", want: DialectPostgres},
		{in: "postgresql", wantErr: true},
		{in: "mysql", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDBDriver(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestRedactDSN(t *testing.T) {
	const secret = "unit-test-pw"
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "url userinfo", dsn: "postgres://postgres:" + secret + "@127.0.0.1:15432/defect_drainer?sslmode=prefer"},
		{name: "url query", dsn: "postgres://postgres@127.0.0.1:15432/defect_drainer?sslmode=prefer&password=" + secret},
		{name: "keyword", dsn: "host=127.0.0.1 port=15432 user=postgres password=" + secret + " dbname=defect_drainer"},
		{name: "keyword quoted", dsn: "host=127.0.0.1 password='" + secret + " extra' dbname=defect_drainer"},
		{name: "sslpassword", dsn: "host=127.0.0.1 sslpassword=" + secret + " dbname=defect_drainer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactDSN(tt.dsn)
			if strings.Contains(got, secret) {
				t.Fatalf("redacted DSN still contains the password: %s", got)
			}
			if !strings.Contains(got, "xxxxx") {
				t.Fatalf("redacted DSN has no xxxxx marker: %s", got)
			}
			if !strings.Contains(got, "127.0.0.1") {
				t.Fatalf("redacted DSN dropped the host: %s", got)
			}
		})
	}

	plain := "postgres://postgres@127.0.0.1:15432/defect_drainer?sslmode=prefer"
	if got := redactDSN(plain); strings.Contains(got, "xxxxx") {
		t.Fatalf("DSN without a password was rewritten: %s", got)
	}
	if got := redactDSN(""); got != "" {
		t.Fatalf("empty DSN: %q", got)
	}
}

func TestScrubTextRemovesPassword(t *testing.T) {
	const secret = "unit-test-pw"
	dsn := "postgres://postgres:" + secret + "@127.0.0.1:15432/defect_drainer?sslmode=prefer"
	msg := "failed to connect to " + dsn + " password=" + secret
	got := scrubText(msg, dsn)
	if strings.Contains(got, secret) {
		t.Fatalf("scrubbed error still contains the password: %s", got)
	}
	if strings.Contains(got, dsn) {
		t.Fatalf("scrubbed error still contains the raw DSN: %s", got)
	}
}

func TestOpenConfiguredSQLite(t *testing.T) {
	t.Setenv("DEFECT_DRAINER_DB_DRIVER", "")
	t.Setenv("DEFECT_CHANNEL_DB_DRIVER", "")
	dir := t.TempDir()
	sqlDB, dialect, err := OpenConfigured(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if dialect != DialectSQLite {
		t.Fatalf("dialect %q", dialect)
	}
	if IsPostgres(sqlDB) {
		t.Fatal("sqlite connection reported as postgres")
	}
	if _, err := os.Stat(filepath.Join(dir, "defect-drainer.db")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenConfiguredRejectsUnknownDriver(t *testing.T) {
	t.Setenv("DEFECT_DRAINER_DB_DRIVER", "mysql")
	t.Setenv("DEFECT_CHANNEL_DB_DRIVER", "")
	dir := t.TempDir()
	_, _, err := OpenConfigured(dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "defect-drainer.db")); !os.IsNotExist(statErr) {
		t.Fatalf("unknown driver created a sqlite file: %v", statErr)
	}
}

func TestIsPostgresDriverIdentity(t *testing.T) {
	// sql.Open does not dial. Do not Ping.
	sqlDB, err := sql.Open(qmarkDriverName, "postgres://postgres:unit-test-pw@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if !IsPostgres(sqlDB) {
		t.Fatal("pgx-qmark connection was not detected")
	}
	if IsPostgres(nil) {
		t.Fatal("nil db")
	}
}

func TestQmarkConnRewritesAndForwards(t *testing.T) {
	inner := &captureConn{}
	c := &qmarkConn{inner: inner}
	if _, err := c.PrepareContext(context.Background(), "SELECT ?"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(context.Background(), "UPDATE t SET a = ? WHERE id = ?", nil); err != nil {
		t.Fatal(err)
	}
	_, _ = c.QueryContext(context.Background(), "SELECT a FROM t WHERE b = ?", nil)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.ResetSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckNamedValue(&driver.NamedValue{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"preparectx:SELECT $1",
		"exec:UPDATE t SET a = $1 WHERE id = $2",
		"query:SELECT a FROM t WHERE b = $1",
		"ping",
		"reset",
		"check",
	}
	if strings.Join(inner.got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", inner.got)
	}
}

func TestQmarkConnectorScrubsConnectError(t *testing.T) {
	const secret = "unit-test-pw"
	dsn := "postgres://postgres:" + secret + "@127.0.0.1:1/none"
	c := &qmarkConnector{
		inner: errConnector{err: errors.New("dial " + dsn)},
		dsn:   dsn,
	}
	_, err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("connect error leaked the password: %s", err.Error())
	}
}

func TestWrapperForwardsPgxConnInterfaces(t *testing.T) {
	inner := reflect.TypeOf((*stdlib.Conn)(nil))
	wrap := reflect.TypeOf((*qmarkConn)(nil))
	for _, name := range []string{
		"PrepareContext", "ExecContext", "QueryContext", "BeginTx",
		"Ping", "ResetSession", "CheckNamedValue", "IsValid",
	} {
		_, hasInner := inner.MethodByName(name)
		_, hasWrap := wrap.MethodByName(name)
		if hasInner && !hasWrap {
			t.Errorf("stdlib.Conn implements %s; qmarkConn does not forward it", name)
		}
	}
	// pgx stdlib.Conn has no IsValid. Advertising driver.Validator would make
	// database/sql keep the conn on rollback (it requires ResetSession and Validator).
	_, pgxValid := inner.MethodByName("IsValid")
	_, wrapValid := wrap.MethodByName("IsValid")
	if wrapValid != pgxValid {
		t.Errorf("IsValid forwarding mismatch: pgx %v wrapper %v", pgxValid, wrapValid)
	}
}

type captureConn struct {
	got []string
}

func (c *captureConn) Prepare(query string) (driver.Stmt, error) {
	c.got = append(c.got, "prepare:"+query)
	return nopStmt{}, nil
}

func (c *captureConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	c.got = append(c.got, "preparectx:"+query)
	return nopStmt{}, nil
}

func (c *captureConn) Close() error { return nil }

func (c *captureConn) Begin() (driver.Tx, error) { return nil, errors.New("no tx") }

func (c *captureConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.got = append(c.got, "begin")
	return nil, driver.ErrSkip
}

func (c *captureConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.got = append(c.got, "exec:"+query)
	return driver.RowsAffected(0), nil
}

func (c *captureConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.got = append(c.got, "query:"+query)
	return nil, errors.New("no rows")
}

func (c *captureConn) Ping(context.Context) error {
	c.got = append(c.got, "ping")
	return nil
}

func (c *captureConn) ResetSession(context.Context) error {
	c.got = append(c.got, "reset")
	return nil
}

func (c *captureConn) CheckNamedValue(*driver.NamedValue) error {
	c.got = append(c.got, "check")
	return nil
}

type nopStmt struct{}

func (nopStmt) Close() error  { return nil }
func (nopStmt) NumInput() int { return -1 }
func (nopStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("deprecated")
}
func (nopStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("deprecated")
}

type errConnector struct{ err error }

func (c errConnector) Connect(context.Context) (driver.Conn, error) { return nil, c.err }
func (c errConnector) Driver() driver.Driver                        { return nil }
