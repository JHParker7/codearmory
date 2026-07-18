package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The sql backend provisions a per-service database from one maintenance connection
// (a role with CREATEDB, plus CREATEROLE unless the CREATEDB-only profile is used). It
// runs ONCE at enable time: the derived per-service URL is stored encrypted like a
// manual db_url, so the reconciler treats the service as manual thereafter and the
// maintenance credential need not persist. It NEVER drops a database or role.

// dbIdentifierRe matches a safe lowercase SQL identifier. Service names from the catalog
// (e.g. "gitea_integration") already conform. Validated identifiers contain no double
// quote, so quoteIdent can wrap them without an injection surface.
var dbIdentifierRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func validDBIdentifier(s string) bool {
	return s != "" && len(s) <= 63 && dbIdentifierRe.MatchString(s)
}

// serviceDBName is the database name for a service (its lowercased name).
func serviceDBName(service string) string {
	return strings.ToLower(strings.TrimSpace(service))
}

// roleNameForService is the per-service login role builder creates in the create-role profile.
func roleNameForService(service string) string {
	return "svc_" + serviceDBName(service)
}

// quoteIdent double-quotes a SQL identifier. The caller MUST have validated it with
// validDBIdentifier; this only wraps (a validated identifier cannot contain a quote).
func quoteIdent(s string) string { return `"` + s + `"` }

// generateDBPassword returns a 32-byte base64url (no padding) password. Its alphabet
// (A-Za-z0-9-_) has no SQL string-literal metacharacters, so it embeds safely in a
// CREATE ROLE ... PASSWORD '...' literal with no escaping needed.
func generateDBPassword() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// pgMaintenanceConn is builder's minimal view of a maintenance Postgres connection,
// kept narrow so provisionSQLDatabase is unit-tested against a fake.
type pgMaintenanceConn interface {
	rolePrivileges(ctx context.Context) (createDB, createRole, super bool, err error)
	roleExists(ctx context.Context, name string) (bool, error)
	databaseExists(ctx context.Context, name string) (bool, error)
	exec(ctx context.Context, ddl string) error
	close(ctx context.Context)
}

// dialPG opens a maintenance connection; overridable in tests.
var dialPG = func(ctx context.Context, dsn string) (pgMaintenanceConn, error) {
	return dialPGX(ctx, dsn)
}

// provisionSQLDatabase ensures the per-service role + database exist on the maintenance
// connection and returns the derived per-service connection URL. Idempotent; never drops.
func provisionSQLDatabase(ctx context.Context, maintenanceURL, service string, cfg dbConfig) (string, error) {
	dbName := serviceDBName(service)
	if !validDBIdentifier(dbName) {
		return "", fmt.Errorf("service %q is not a valid database identifier", service)
	}

	conn, err := dialPG(ctx, maintenanceURL)
	if err != nil {
		return "", fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer conn.close(ctx)

	createDB, createRole, super, err := conn.rolePrivileges(ctx)
	if err != nil {
		return "", fmt.Errorf("check maintenance role privileges: %w", err)
	}
	if super {
		return "", errors.New("refusing to use a superuser maintenance role — grant only CREATEDB (+CREATEROLE)")
	}
	if !createDB {
		return "", errors.New("maintenance role lacks CREATEDB")
	}

	role, password, err := ensureOwnerRole(ctx, conn, service, createRole, cfg)
	if err != nil {
		return "", err
	}

	dbExists, err := conn.databaseExists(ctx, dbName)
	if err != nil {
		return "", fmt.Errorf("check database %s: %w", dbName, err)
	}
	if !dbExists {
		// CREATE DATABASE cannot run inside a transaction block — issue it standalone.
		if err := conn.exec(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s", quoteIdent(dbName), quoteIdent(role))); err != nil {
			return "", fmt.Errorf("create database %s: %w", dbName, err)
		}
	}

	return derivePerServiceURL(maintenanceURL, role, password, dbName, cfg.sqlSSLMode)
}

// ensureOwnerRole resolves the database owner role and the password to put in the
// derived URL. In the create-role profile it ensures a per-service login role with a
// freshly generated password; in the CREATEDB-only profile it uses the admin's
// pre-created owner role + password and creates no role.
func ensureOwnerRole(ctx context.Context, conn pgMaintenanceConn, service string, createRole bool, cfg dbConfig) (role, password string, err error) {
	if !cfg.sqlCreateRole {
		role = strings.ToLower(strings.TrimSpace(cfg.sqlOwnerRole))
		if !validDBIdentifier(role) {
			return "", "", errors.New("CREATEDB-only profile requires a valid BUILDER_DB_SQL_OWNER_ROLE")
		}
		if cfg.sqlOwnerPassword == "" {
			return "", "", errors.New("CREATEDB-only profile requires BUILDER_DB_SQL_OWNER_PASSWORD for the derived URL")
		}
		return role, cfg.sqlOwnerPassword, nil
	}

	role = roleNameForService(service)
	if !validDBIdentifier(role) {
		return "", "", fmt.Errorf("derived role %q is invalid", role)
	}
	if !createRole {
		return "", "", errors.New("maintenance role lacks CREATEROLE (set BUILDER_DB_SQL_CREATE_ROLE=false to use a shared owner role)")
	}
	password, err = generateDBPassword()
	if err != nil {
		return "", "", err
	}
	exists, err := conn.roleExists(ctx, role)
	if err != nil {
		return "", "", fmt.Errorf("check role %s: %w", role, err)
	}
	verb := "CREATE"
	if exists {
		// The role already exists but we cannot read its password — set a known one so
		// the derived URL works. (At enable the pod is not yet running, so no churn.)
		verb = "ALTER"
	}
	if err := conn.exec(ctx, fmt.Sprintf("%s ROLE %s WITH LOGIN PASSWORD '%s'", verb, quoteIdent(role), password)); err != nil {
		return "", "", fmt.Errorf("%s role %s: %w", strings.ToLower(verb), role, err)
	}
	return role, password, nil
}

// derivePerServiceURL builds the per-service connection URL from the maintenance URL,
// swapping in the service role's credentials and database name and preserving host/port
// and query options (forcing sslmode when configured).
func derivePerServiceURL(maintenanceURL, user, password, dbName, sslmode string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(maintenanceURL))
	if err != nil {
		return "", fmt.Errorf("parse maintenance url: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", errors.New("maintenance url scheme must be postgres:// or postgresql://")
	}
	u.User = url.UserPassword(user, password)
	u.Path = "/" + dbName
	if sslmode != "" {
		q := u.Query()
		q.Set("sslmode", sslmode)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// --- pgx-backed maintenance connection ---

type pgxConn struct{ conn *pgx.Conn }

func dialPGX(ctx context.Context, dsn string) (pgMaintenanceConn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &pgxConn{conn: conn}, nil
}

func (c *pgxConn) rolePrivileges(ctx context.Context) (createDB, createRole, super bool, err error) {
	err = c.conn.QueryRow(ctx,
		"SELECT rolcreatedb, rolcreaterole, rolsuper FROM pg_roles WHERE rolname = current_user").
		Scan(&createDB, &createRole, &super)
	return
}

func (c *pgxConn) roleExists(ctx context.Context, name string) (bool, error) {
	return c.existsRow(ctx, "SELECT 1 FROM pg_roles WHERE rolname = $1", name)
}

func (c *pgxConn) databaseExists(ctx context.Context, name string) (bool, error) {
	return c.existsRow(ctx, "SELECT 1 FROM pg_database WHERE datname = $1", name)
}

func (c *pgxConn) existsRow(ctx context.Context, query, arg string) (bool, error) {
	var x int
	err := c.conn.QueryRow(ctx, query, arg).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *pgxConn) exec(ctx context.Context, ddl string) error {
	_, err := c.conn.Exec(ctx, ddl)
	return err
}

func (c *pgxConn) close(ctx context.Context) { _ = c.conn.Close(ctx) }
