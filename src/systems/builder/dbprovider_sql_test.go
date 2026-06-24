package main

import (
	"context"
	"strings"
	"testing"
)

// fakePGConn is an in-memory pgMaintenanceConn for testing provisionSQLDatabase without a
// real database. It records every DDL statement so tests can assert what builder ran.
type fakePGConn struct {
	createDB, createRole, super bool
	roles                       map[string]bool
	dbs                         map[string]bool
	execs                       []string
}

func (c *fakePGConn) rolePrivileges(context.Context) (bool, bool, bool, error) {
	return c.createDB, c.createRole, c.super, nil
}
func (c *fakePGConn) roleExists(_ context.Context, name string) (bool, error) {
	return c.roles[name], nil
}
func (c *fakePGConn) databaseExists(_ context.Context, name string) (bool, error) {
	return c.dbs[name], nil
}
func (c *fakePGConn) exec(_ context.Context, ddl string) error {
	c.execs = append(c.execs, ddl)
	return nil
}
func (c *fakePGConn) close(context.Context) {}

// withFakeDial installs a fake dialer returning conn and restores the real one after.
func withFakeDial(t *testing.T, conn pgMaintenanceConn) {
	t.Helper()
	orig := dialPG
	dialPG = func(context.Context, string) (pgMaintenanceConn, error) { return conn, nil }
	t.Cleanup(func() { dialPG = orig })
}

func hasExecContaining(execs []string, substr string) bool {
	for _, e := range execs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func TestProvisionSQL_CreateRoleProfile(t *testing.T) {
	conn := &fakePGConn{createDB: true, createRole: true, roles: map[string]bool{}, dbs: map[string]bool{}}
	withFakeDial(t, conn)

	cfg := dbConfig{sqlCreateRole: true}
	url, err := provisionSQLDatabase(context.Background(), "postgres://admin:pw@db:5432/postgres", "blueprints", cfg)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !hasExecContaining(conn.execs, `CREATE ROLE "svc_blueprints"`) {
		t.Errorf("expected CREATE ROLE, got %v", conn.execs)
	}
	if !hasExecContaining(conn.execs, `CREATE DATABASE "blueprints" OWNER "svc_blueprints"`) {
		t.Errorf("expected CREATE DATABASE, got %v", conn.execs)
	}
	// Derived URL points at the new role + database, keeping host:port.
	if !strings.HasPrefix(url, "postgres://svc_blueprints:") {
		t.Errorf("derived url user wrong: %s", url)
	}
	if !strings.Contains(url, "@db:5432/blueprints") {
		t.Errorf("derived url host/db wrong: %s", url)
	}
}

func TestProvisionSQL_Idempotent(t *testing.T) {
	// Role and database already exist: builder must ALTER the role (not CREATE) and skip
	// creating the database.
	conn := &fakePGConn{
		createDB: true, createRole: true,
		roles: map[string]bool{"svc_forge": true},
		dbs:   map[string]bool{"forge": true},
	}
	withFakeDial(t, conn)

	if _, err := provisionSQLDatabase(context.Background(), "postgres://a:b@h/postgres", "forge", dbConfig{sqlCreateRole: true}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if hasExecContaining(conn.execs, "CREATE ROLE") {
		t.Errorf("should not CREATE an existing role: %v", conn.execs)
	}
	if !hasExecContaining(conn.execs, `ALTER ROLE "svc_forge"`) {
		t.Errorf("expected ALTER ROLE for existing role: %v", conn.execs)
	}
	if hasExecContaining(conn.execs, "CREATE DATABASE") {
		t.Errorf("should not CREATE an existing database: %v", conn.execs)
	}
}

func TestProvisionSQL_RefusesSuperuser(t *testing.T) {
	conn := &fakePGConn{createDB: true, createRole: true, super: true}
	withFakeDial(t, conn)
	_, err := provisionSQLDatabase(context.Background(), "postgres://a:b@h/postgres", "forge", dbConfig{sqlCreateRole: true})
	if err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("expected superuser refusal, got %v", err)
	}
}

func TestProvisionSQL_RequiresCreateDB(t *testing.T) {
	conn := &fakePGConn{createDB: false, createRole: true}
	withFakeDial(t, conn)
	_, err := provisionSQLDatabase(context.Background(), "postgres://a:b@h/postgres", "forge", dbConfig{sqlCreateRole: true})
	if err == nil || !strings.Contains(err.Error(), "CREATEDB") {
		t.Fatalf("expected CREATEDB error, got %v", err)
	}
}

func TestProvisionSQL_CreateRoleProfileNeedsCreateRole(t *testing.T) {
	conn := &fakePGConn{createDB: true, createRole: false, roles: map[string]bool{}, dbs: map[string]bool{}}
	withFakeDial(t, conn)
	_, err := provisionSQLDatabase(context.Background(), "postgres://a:b@h/postgres", "forge", dbConfig{sqlCreateRole: true})
	if err == nil || !strings.Contains(err.Error(), "CREATEROLE") {
		t.Fatalf("expected CREATEROLE error, got %v", err)
	}
}

func TestProvisionSQL_CreateDBOnlyProfile(t *testing.T) {
	// CREATEDB-only: no per-service role is created; the admin's owner role + password are
	// used for the derived URL, and only CREATE DATABASE runs.
	conn := &fakePGConn{createDB: true, createRole: false, roles: map[string]bool{}, dbs: map[string]bool{}}
	withFakeDial(t, conn)
	cfg := dbConfig{sqlCreateRole: false, sqlOwnerRole: "apps", sqlOwnerPassword: "ownerpw"}
	url, err := provisionSQLDatabase(context.Background(), "postgres://admin:pw@db:5432/postgres", "tickets", cfg)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if hasExecContaining(conn.execs, "CREATE ROLE") || hasExecContaining(conn.execs, "ALTER ROLE") {
		t.Errorf("CREATEDB-only profile must not touch roles: %v", conn.execs)
	}
	if !hasExecContaining(conn.execs, `CREATE DATABASE "tickets" OWNER "apps"`) {
		t.Errorf("expected CREATE DATABASE owned by apps: %v", conn.execs)
	}
	if !strings.HasPrefix(url, "postgres://apps:ownerpw@") {
		t.Errorf("derived url should use owner creds: %s", url)
	}
}

func TestProvisionSQL_CreateDBOnlyRequiresOwnerPassword(t *testing.T) {
	conn := &fakePGConn{createDB: true}
	withFakeDial(t, conn)
	_, err := provisionSQLDatabase(context.Background(), "postgres://a:b@h/postgres", "tickets",
		dbConfig{sqlCreateRole: false, sqlOwnerRole: "apps"})
	if err == nil || !strings.Contains(err.Error(), "OWNER_PASSWORD") {
		t.Fatalf("expected owner-password error, got %v", err)
	}
}

func TestProvisionSQL_SSLMode(t *testing.T) {
	conn := &fakePGConn{createDB: true, createRole: true, roles: map[string]bool{}, dbs: map[string]bool{}}
	withFakeDial(t, conn)
	url, err := provisionSQLDatabase(context.Background(), "postgres://a:b@h:5432/postgres", "hooks",
		dbConfig{sqlCreateRole: true, sqlSSLMode: "require"})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !strings.Contains(url, "sslmode=require") {
		t.Errorf("expected sslmode=require in derived url: %s", url)
	}
}

func TestDerivePerServiceURL(t *testing.T) {
	url, err := derivePerServiceURL("postgresql://admin:secret@pg.example.com:5432/postgres?connect_timeout=5", "svc_x", "p@ss/word", "x", "")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// Host, port and existing query are preserved; credentials and db are swapped; the
	// password is percent-encoded by net/url.
	if !strings.Contains(url, "@pg.example.com:5432/x") {
		t.Errorf("host/db not preserved: %s", url)
	}
	if !strings.Contains(url, "connect_timeout=5") {
		t.Errorf("query not preserved: %s", url)
	}
	if strings.Contains(url, "p@ss/word") {
		t.Errorf("password should be percent-encoded, not raw: %s", url)
	}
}

func TestValidDBIdentifier(t *testing.T) {
	ok := []string{"blueprints", "gitea_integration", "svc_forge", "a", "a1_b2"}
	bad := []string{"", "1abc", "Forge", "a-b", "a;b", "drop table", strings.Repeat("a", 64)}
	for _, s := range ok {
		if !validDBIdentifier(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range bad {
		if validDBIdentifier(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestGenerateDBPassword(t *testing.T) {
	a, err := generateDBPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generateDBPassword()
	if a == b {
		t.Error("passwords should be unique")
	}
	// base64url alphabet only — nothing that needs SQL-literal escaping.
	if strings.ContainsAny(a, `'"\ `) {
		t.Errorf("password has unsafe characters: %s", a)
	}
}
