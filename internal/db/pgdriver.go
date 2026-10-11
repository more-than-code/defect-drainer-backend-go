package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"

	"github.com/jackc/pgx/v5/stdlib"
)

// qmarkDriverName is the database/sql driver that speaks pgx and rewrites ?.
const qmarkDriverName = "pgx-qmark"

func init() {
	sql.Register(qmarkDriverName, &qmarkDriver{inner: stdlib.GetDefaultDriver()})
}

// qmarkDriver wraps pgx's stdlib driver so callers can keep writing ?.
type qmarkDriver struct {
	inner driver.Driver
}

func (d *qmarkDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, scrubErr(err, name)
	}
	return &qmarkConn{inner: conn}, nil
}

func (d *qmarkDriver) OpenConnector(name string) (driver.Connector, error) {
	var inner driver.Connector
	if dc, ok := d.inner.(driver.DriverContext); ok {
		c, err := dc.OpenConnector(name)
		if err != nil {
			return nil, scrubErr(err, name)
		}
		inner = c
	} else {
		inner = dsnConnector{driver: d.inner, dsn: name}
	}
	return &qmarkConnector{driver: d, inner: inner, dsn: name}, nil
}

type dsnConnector struct {
	driver driver.Driver
	dsn    string
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c dsnConnector) Driver() driver.Driver { return c.driver }

type qmarkConnector struct {
	driver *qmarkDriver
	inner  driver.Connector
	dsn    string
}

func (c *qmarkConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, scrubErr(err, c.dsn)
	}
	return &qmarkConn{inner: conn}, nil
}

func (c *qmarkConnector) Driver() driver.Driver { return c.driver }

func scrubErr(err error, dsn string) error {
	if err == nil {
		return nil
	}
	return errors.New(scrubText(err.Error(), dsn))
}

// qmarkConn rewrites SQL on the interfaces database/sql actually calls,
// then forwards. ExecContext and QueryContext return ErrSkip when the inner
// conn lacks them so database/sql falls back to Prepare. ResetSession and
// Ping return nil in that case: a non-nil error from those drops the conn.
type qmarkConn struct {
	inner driver.Conn
}

func (c *qmarkConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(RebindDollar(query))
}

func (c *qmarkConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	q := RebindDollar(query)
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, q)
	}
	return c.inner.Prepare(q)
}

func (c *qmarkConn) Close() error { return c.inner.Close() }

func (c *qmarkConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

func (c *qmarkConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	// database/sql does not treat ErrSkip as a fallback once ConnBeginTx exists.
	// pgx implements it; this branch is only for a driver that does not.
	if opts.Isolation != 0 || opts.ReadOnly {
		return nil, errors.New("db: driver does not support transaction options")
	}
	return c.inner.Begin()
}

func (c *qmarkConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ec.ExecContext(ctx, RebindDollar(query), args)
}

func (c *qmarkConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qc.QueryContext(ctx, RebindDollar(query), args)
}

func (c *qmarkConn) Ping(ctx context.Context) error {
	p, ok := c.inner.(driver.Pinger)
	if !ok {
		return nil
	}
	return p.Ping(ctx)
}

func (c *qmarkConn) ResetSession(ctx context.Context) error {
	sr, ok := c.inner.(driver.SessionResetter)
	if !ok {
		return nil
	}
	return sr.ResetSession(ctx)
}

func (c *qmarkConn) CheckNamedValue(nv *driver.NamedValue) error {
	n, ok := c.inner.(driver.NamedValueChecker)
	if !ok {
		return driver.ErrSkip
	}
	return n.CheckNamedValue(nv)
}
