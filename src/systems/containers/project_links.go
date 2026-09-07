package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ProjectLink associates a container repository with a gatekeeper Project.
//
// A container repository is not persisted — it is `namespace/image`, proxied live
// from the upstream registry — so there is no repository row to carry a project id.
// This table is that missing row, and nothing more: the minimum state needed to
// answer "which project, if any, owns this repository?" for the authz fallback and
// the list filter.
//
// Granularity is (Namespace, Image):
//   - Image set   → the link binds exactly namespace/image.
//   - Image empty → the link binds the WHOLE namespace (every image under it).
//
// The unique index on (namespace, image) makes a repository (or a namespace) map to
// at most one project; re-stamping upserts. A repository with no matching row is
// unlinked and behaves exactly as before this table existed.
type ProjectLink struct {
	ID string `json:"id" gorm:"column:id;primaryKey"`
	// Namespace is the container namespace (registry first path segment). Required.
	Namespace string `json:"namespace" gorm:"column:namespace;not null;uniqueIndex:idx_container_project_ns_image,priority:1"`
	// Image is the repository name within the namespace, or "" for a namespace-wide
	// link. Part of the unique key so ("ns","") and ("ns","img") can coexist.
	Image string `json:"image" gorm:"column:image;not null;default:'';uniqueIndex:idx_container_project_ns_image,priority:2"`
	// Project is the gatekeeper project slug the repository is filed into.
	Project string `json:"project" gorm:"column:project;not null;default:''"`
	// ProjectID is the resolved gatekeeper project id (stable across a slug rename)
	// and the key list views widen on.
	ProjectID string `json:"project_id" gorm:"column:project_id;not null;default:'';index"`
	// ProjectNamespace is the project owner's namespace, recorded for parity with
	// forge/tickets (the namespace a member's project grant is qualified with).
	ProjectNamespace string    `json:"project_namespace" gorm:"column:project_namespace;not null;default:''"`
	CreatedAt        time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt        time.Time `json:"updated_at" gorm:"column:updated_at"`
}

// TableName sets the GORM table name for ProjectLink.
func (ProjectLink) TableName() string { return "container_project_links" }

// upsertProjectLink stamps (or re-stamps) the link for (namespace, image) onto the
// given project, keyed by the unique (namespace, image) index.
func upsertProjectLink(ctx context.Context, link ProjectLink) error {
	ctx, span := otel.Tracer("containers").Start(ctx, "db.project_link.upsert")
	defer span.End()
	span.SetAttributes(
		attribute.String("repo.namespace", link.Namespace),
		attribute.String("repo.image", link.Image),
		attribute.String("project.slug", link.Project),
	)
	err := connect().WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "namespace"}, {Name: "image"}},
		DoUpdates: clause.AssignmentColumns([]string{"project", "project_id", "project_namespace", "updated_at"}),
	}).Create(&link).Error
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// deleteProjectLink removes the link for (namespace, image). Deleting a link that
// does not exist is not an error — the repository is simply unlinked.
func deleteProjectLink(ctx context.Context, namespace, image string) error {
	ctx, span := otel.Tracer("containers").Start(ctx, "db.project_link.delete")
	defer span.End()
	span.SetAttributes(
		attribute.String("repo.namespace", namespace),
		attribute.String("repo.image", image),
	)
	err := connect().WithContext(ctx).
		Where("namespace = ? AND image = ?", namespace, image).
		Delete(&ProjectLink{}).Error
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// linksForNamespace returns every link that could govern a repository under
// namespace: the namespace-wide link (image "") and any image-specific links. Uses
// the fail-closed read connection so a database blip on the authorization hot path
// degrades to "unlinked" rather than crashing.
func linksForNamespace(ctx context.Context, namespace string) ([]ProjectLink, error) {
	conn, err := connectReadSafe()
	if err != nil {
		return nil, err
	}
	var links []ProjectLink
	err = conn.WithContext(ctx).
		Where("namespace = ?", namespace).
		Find(&links).Error
	return links, err
}

// linksForProject returns every link filed into the given gatekeeper project id.
func linksForProject(ctx context.Context, projectID string) ([]ProjectLink, error) {
	conn, err := connectReadSafe()
	if err != nil {
		return nil, err
	}
	var links []ProjectLink
	err = conn.WithContext(ctx).
		Where("project_id = ?", projectID).
		Find(&links).Error
	return links, err
}

// lookupRepoProjectLink returns the single link that governs namespace/image,
// preferring an exact image link over a namespace-wide one. ok is false when the
// repository is unlinked.
func lookupRepoProjectLink(ctx context.Context, namespace, image string) (ProjectLink, bool, error) {
	links, err := linksForNamespace(ctx, namespace)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ProjectLink{}, false, nil
		}
		return ProjectLink{}, false, err
	}
	link, ok := selectLink(links, image)
	return link, ok, nil
}

// selectLink picks the link governing image from a namespace's links: an exact
// image match wins over the namespace-wide (image "") link. Pure so the precedence
// rule can be tested without a database.
func selectLink(links []ProjectLink, image string) (ProjectLink, bool) {
	var wide ProjectLink
	haveWide := false
	for _, l := range links {
		if l.Image == image {
			return l, true
		}
		if l.Image == "" {
			wide = l
			haveWide = true
		}
	}
	if haveWide {
		return wide, true
	}
	return ProjectLink{}, false
}

// filterCatalogByLinks narrows a live registry catalog ("namespace/image" strings)
// to those governed by one of links — an image-specific link matches that exact
// repository, a namespace-wide link matches every repository under its namespace.
// Pure so list filtering can be tested without a registry or a database.
func filterCatalogByLinks(catalog []string, links []ProjectLink) []string {
	if len(links) == 0 {
		return []string{}
	}
	wideNS := make(map[string]bool)
	exact := make(map[string]bool)
	for _, l := range links {
		if l.Image == "" {
			wideNS[l.Namespace] = true
		} else {
			exact[l.Namespace+"/"+l.Image] = true
		}
	}
	out := make([]string, 0, len(catalog))
	for _, repo := range catalog {
		ns, _, _ := strings.Cut(repo, "/")
		if wideNS[ns] || exact[repo] {
			out = append(out, repo)
		}
	}
	return out
}
