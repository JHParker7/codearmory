package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// Deploying a specific build.
//
// PUT /services/{service} is a whole-object write: it rewrites config, port,
// description and the rest from the request body. That is the right shape for an
// admin editing a service, and the wrong shape for a CI pipeline that knows one
// fact — "the image I just pushed is tagged X". Sending a partial PUT from a
// pipeline would blank every field it omitted.
//
// PATCH /services/{service}/image is that narrow write. It touches image, tag and
// pull_policy only, leaves everything else on the row untouched, and nudges the
// reconciler so the new build rolls out immediately rather than at the next tick.

// validPullPolicies are the only imagePullPolicy values Kubernetes accepts. An
// invalid one is rejected here rather than at apply time, where it would surface as
// a rejected Deployment long after the request succeeded.
var validPullPolicies = map[string]bool{"Always": true, "IfNotPresent": true, "Never": true}

// tagPattern is the docker tag grammar: up to 128 of [A-Za-z0-9_.-], not opening
// with a separator. Validated because the tag is interpolated into an image
// reference — a value containing "/" or ":" would silently retarget the pull.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// setImageRequest is the PATCH body. Every field is optional; an omitted field is
// left as-is, and an explicit empty string CLEARS the override (falling back to the
// platform default). That distinction is why these are pointers.
type setImageRequest struct {
	Image *string `json:"image"`
	// Registry retargets which registry this service's images come from, so a tag is
	// usable on an install whose platform-wide registry is not where this service
	// publishes. Without it, Tag is only meaningful when the global happens to match.
	Registry   *string `json:"registry"`
	Tag        *string `json:"tag"`
	PullPolicy *string `json:"pull_policy"`
}

func validateImageRequest(req setImageRequest) string {
	if req.Tag != nil && *req.Tag != "" && !tagPattern.MatchString(*req.Tag) {
		return "tag must match [A-Za-z0-9_][A-Za-z0-9_.-]{0,127}"
	}
	if req.PullPolicy != nil && *req.PullPolicy != "" && !validPullPolicies[*req.PullPolicy] {
		return "pull_policy must be one of Always, IfNotPresent, Never"
	}
	if req.Image != nil && strings.ContainsAny(*req.Image, " \t\n") {
		return "image must not contain whitespace"
	}
	if req.Registry != nil && *req.Registry != "" {
		r := *req.Registry
		// A scheme would produce "https:/host/repo:tag" once composed; a trailing slash
		// would double up. Both are silent misconfigurations, so reject them here.
		if strings.Contains(r, "://") || strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") {
			return "registry must be a host[:port][/path] with no scheme and no leading or trailing slash"
		}
		if strings.ContainsAny(r, " \t\n") {
			return "registry must not contain whitespace"
		}
	}
	return ""
}

// applyImageRequest folds a retarget request into the stored row. Split out from the
// handler so the precedence rule below is testable without a database.
func applyImageRequest(existing *OrgService, req setImageRequest) {
	if req.Image != nil {
		existing.Image = *req.Image
	}
	if req.Registry != nil {
		existing.Registry = *req.Registry
	}
	if req.Tag != nil {
		existing.Tag = *req.Tag
	}
	if req.PullPolicy != nil {
		existing.PullPolicy = *req.PullPolicy
	}
	// Retargeting by registry/tag must DROP any stored explicit image. imageFor gives
	// Image strict precedence over Registry+Tag, so leaving a previously-pinned full
	// reference in place makes a tag-only retarget a silent no-op: the row records the
	// new tag, the API returns 200, and the reconciler goes on deploying the old
	// reference. That is precisely the "green pipeline over a stale cluster" this
	// endpoint exists to prevent — and it is invisible, because the failure is a deploy
	// that never happened rather than one that errored.
	//
	// Only when the caller did NOT supply an image: an explicit image alongside a tag
	// is a deliberate full-reference pin and is left exactly as asked.
	if req.Image == nil && (req.Registry != nil || req.Tag != nil) {
		existing.Image = ""
	}
}

func handleSetOrgServiceImage(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("builder").Start(r.Context(), "handleSetOrgServiceImage")
	defer span.End()

	service := r.PathValue("service")
	// Same action/resource convention as the sibling handlers: a builder-scoped
	// resource, not a per-service one, so an existing admin grant covers this too.
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "setOrgServiceImage", builderResource)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	// Default scope, matching handleSetOrgService: these rows are the baseline every
	// org inherits, and it is the scope the reconciler acts on.
	orgID := defaultOrgID
	if service == "" {
		http.Error(w, "service name is required", http.StatusBadRequest)
		return
	}
	if coreServices[service] {
		http.Error(w, "core services cannot be configured", http.StatusBadRequest)
		return
	}

	var req setImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if msg := validateImageRequest(req); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if req.Image == nil && req.Registry == nil && req.Tag == nil && req.PullPolicy == nil {
		http.Error(w, "at least one of image, registry, tag or pull_policy is required", http.StatusBadRequest)
		return
	}

	// The row must already exist: this endpoint retargets a CONFIGURED service, it
	// does not enable one. Creating a service from a CI step would be a surprising
	// amount of authority for "deploy this tag".
	existing, err := getOrgServicePrimary(ctx, orgID, service)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "service is not configured for this scope — configure it first", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		slog.ErrorContext(ctx, "set service image: db read", "org_id", orgID, "service", service, "error", err)
		http.Error(w, "failed to read service config", http.StatusInternalServerError)
		return
	}

	applyImageRequest(&existing, req)

	if _, err := upsertOrgService(ctx, existing); err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "set service image: db write", "org_id", orgID, "service", service, "error", err)
		http.Error(w, "failed to save service image", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(
		attribute.String("org.id", orgID),
		attribute.String("service", service),
		attribute.String("image.tag", existing.Tag),
		attribute.String("image.ref", existing.Image),
	)
	slog.InfoContext(ctx, "service image retargeted",
		"org_id", orgID, "service", service,
		"image", existing.Image, "registry", existing.Registry, "tag", existing.Tag, "pull_policy", existing.PullPolicy,
		"caller_id", userID)

	// The whole point of the endpoint is that the new build is live when it returns,
	// so reconcile now rather than waiting for the next tick.
	if orgID == defaultOrgID {
		reconcilerNudge()
	}

	view, err := effectiveView(ctx, service)
	if err != nil {
		http.Error(w, "saved but failed to read back", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view) //nolint:errcheck
}
