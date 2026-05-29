package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

var pool *pgxpool.Pool

const createTables = `
CREATE TABLE IF NOT EXISTS services (
    service_id   TEXT        PRIMARY KEY,
    name         TEXT        NOT NULL UNIQUE,
    url          TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    forward_auth BOOLEAN     NOT NULL DEFAULT false,
    service_key  TEXT        NOT NULL DEFAULT '',
    active       BOOLEAN     NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE services ADD COLUMN IF NOT EXISTS service_key TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS service_roles (
    role_id     TEXT        PRIMARY KEY,
    service_id  TEXT        NOT NULL REFERENCES services(service_id),
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (service_id, name)
);

CREATE TABLE IF NOT EXISTS service_endpoints (
    endpoint_id TEXT        PRIMARY KEY,
    service_id  TEXT        NOT NULL REFERENCES services(service_id),
    method      TEXT        NOT NULL,
    path        TEXT        NOT NULL,
    action      TEXT        NOT NULL,
    resource    TEXT        NOT NULL,
    public      BOOLEAN     NOT NULL DEFAULT false,
    active      BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS registry_service_accounts (
    account_id  TEXT        PRIMARY KEY,
    name        TEXT        NOT NULL UNIQUE,
    hashed_key  TEXT        NOT NULL,
    role        TEXT        NOT NULL DEFAULT 'read',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func connectDB(ctx context.Context) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgresql://postgres:postgres@localhost:5432/registry"
	}
	p, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	if _, err := p.Exec(ctx, createTables); err != nil {
		slog.Error("failed to create tables", "error", err)
		os.Exit(1)
	}
	pool = p
}
