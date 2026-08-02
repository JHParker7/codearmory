package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// The platform git host (git-factory) has to exist in git_connector as a backend before
// any repo hosted on it can be cloned by a pipeline. That link used to be established at
// runtime by builder, which POSTed to /internal/backends/platform after it deployed
// git-factory. Now that git-factory is a CORE, Helm-deployed service its in-cluster
// address is known at deploy time, so git_connector seeds the row itself from
// GIT_FACTORY_URL and the link no longer depends on builder running at all.
//
// registerPlatformBackend below is the single implementation both paths use: the startup
// seeder and the HTTP endpoint (still served for other platform backends and for an older
// builder that keeps calling it through a rolling upgrade).

const (
	// platformGitFactoryName is the backend row's name. It must stay "git-factory": that
	// is the value builder sent (files/services/git_factory.json → gitConnectorBackend.name),
	// so every existing install already has a platform row under it. Names are unique per
	// owner (ux_git_owner_name), so a different name here would either collide with the
	// existing row or create a second, competing backend.
	platformGitFactoryName = "git-factory"

	// seedRetryInterval is how often seeding is retried before it has ever succeeded —
	// short, because until it lands, clones of platform repos fall back to "no backend
	// for this host".
	seedRetryInterval = 30 * time.Second
	// seedInterval re-runs seeding after a success. Seeding is level-triggered (the same
	// property builder's reconcile pass had): it converges the row on every pass, so an
	// operator deleting it, a DB restore from before the link, or a transient write
	// failure all heal on their own instead of needing a pod restart.
	seedInterval = 10 * time.Minute
)

// platformBackendInputError marks a caller-supplied value as bad (as opposed to an
// internal failure), so the HTTP handler can answer 400 with the same message it used
// to produce inline.
type platformBackendInputError struct{ msg string }

func (e platformBackendInputError) Error() string { return e.msg }

// registerPlatformBackend upserts the credential-free, platform-owned backend for a git
// host reachable at baseURL. It is keyed by host (see upsertPlatformBackend), so calling
// it repeatedly converges one row rather than piling up duplicates.
//
// modeService carries no secret, but the row still goes through sealAuth so every backend
// is stored in one shape and openAuth has something well-formed to read.
func registerPlatformBackend(ctx context.Context, name, baseURL string) (GitBackend, error) {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if name == "" || baseURL == "" {
		return GitBackend{}, platformBackendInputError{"name and base_url are required"}
	}
	host, err := deriveHost(baseURL)
	if err != nil {
		return GitBackend{}, platformBackendInputError{"base_url: " + err.Error()}
	}
	enc, err := sealAuth(authConfig{Mode: modeService})
	if err != nil {
		return GitBackend{}, fmt.Errorf("seal platform auth: %w", err)
	}
	now := time.Now().UTC()
	return upsertPlatformBackend(ctx, GitBackend{
		ID:        uuid.New().String(),
		Owner:     platformOwner,
		Name:      name,
		Type:      backendGitFactory,
		BaseURL:   baseURL,
		Host:      host,
		AuthMode:  modeService,
		AuthEnc:   enc,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// seedPlatformGitFactoryBackend registers the Helm-configured git-factory as the platform
// backend. A no-op when GIT_FACTORY_URL is unset — an install without the platform git
// host simply has no platform row, exactly as before. GIT_FACTORY_INTERNAL_KEY is NOT
// required: the row holds no credential and this path is in-process, so there is no
// east-west call to authenticate (the key still gates the mirror surface in mirror.go).
func seedPlatformGitFactoryBackend(ctx context.Context) error {
	if gitFactoryURL == "" {
		return nil
	}
	ctx, span := otel.Tracer("git").Start(ctx, "seedPlatformGitFactoryBackend")
	defer span.End()
	b, err := registerPlatformBackend(ctx, platformGitFactoryName, gitFactoryURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	slog.DebugContext(ctx, "platform git backend seeded", "backend_id", b.ID, "host", b.Host)
	return nil
}

// startPlatformBackendSeeder keeps the platform git-factory backend registered for the
// lifetime of the process. Best-effort, like StartKeyRotation: a database that is not
// ready yet, or a git-factory URL that does not resolve, must never stop git_connector
// from serving — every other backend still works. Failures are logged and retried on the
// next tick instead of being fatal.
func startPlatformBackendSeeder(ctx context.Context) {
	if gitFactoryURL == "" {
		slog.Info("GIT_FACTORY_URL not set — no platform git backend will be registered")
		return
	}
	go func() {
		seeded := false
		for {
			if err := seedPlatformGitFactoryBackend(ctx); err != nil {
				slog.WarnContext(ctx, "could not register the platform git backend, will retry",
					"base_url", gitFactoryURL, "retry_in", seedRetryInterval, "error", err)
			} else if !seeded {
				seeded = true
				slog.InfoContext(ctx, "platform git backend registered", "base_url", gitFactoryURL, "name", platformGitFactoryName)
			}
			wait := seedRetryInterval
			if seeded {
				wait = seedInterval
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}
