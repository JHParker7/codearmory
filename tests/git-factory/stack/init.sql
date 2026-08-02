-- Databases the stack needs (Postgres does not auto-create from a DSN; apps
-- AutoMigrate their tables into an existing database). Runs once on first boot.
CREATE DATABASE gatekeeper;
CREATE DATABASE registry;
CREATE DATABASE codearmory_git_factory;
