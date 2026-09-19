package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

//go:embed openapi.yaml
var conductorOpenAPIYAML []byte

// handleDocs serves a landing page listing every registered service's docs.
// The list is built from servicesMap at request time so it tracks service
// availability automatically — removing a service from the registry also
// removes its entry here.
func handleDocs(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		name string
		desc string
	}

	routingMu.RLock()
	entries := make([]entry, 0, len(servicesMap)+1)
	for name, svc := range servicesMap {
		entries = append(entries, entry{name: name, desc: svc.description})
	}
	routingMu.RUnlock()

	entries = append(entries, entry{
		name: "conductor",
		desc: "API gateway — routing, auth forwarding, and RBAC enforcement",
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var rows strings.Builder
	for _, e := range entries {
		rows.WriteString(`<li><a href="/docs/` + html.EscapeString(e.name) + `">` + html.EscapeString(e.name) + `</a>`)
		if e.desc != "" {
			rows.WriteString(` — ` + html.EscapeString(e.desc))
		}
		rows.WriteString("</li>\n    ")
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, docsLandingHTML, rows.String())
}

// handleServiceDocs serves the Swagger UI page for a single service.
// The spec URL is an absolute path so the browser resolves it correctly
// regardless of trailing-slash behaviour.
func handleServiceDocs(w http.ResponseWriter, r *http.Request) {
	svc := r.PathValue("service")
	if svc != "conductor" {
		routingMu.RLock()
		_, ok := servicesMap[svc]
		routingMu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, serviceSwaggerHTML, "/docs/"+url.PathEscape(svc)+"/openapi.yaml")
}

// handleServiceSpec proxies GET /docs/{service}/openapi.yaml to the service's
// own GET /openapi.yaml endpoint. For conductor itself it serves the embedded
// spec directly.
func handleServiceSpec(w http.ResponseWriter, r *http.Request) {
	svc := r.PathValue("service")
	if svc == "conductor" {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(conductorOpenAPIYAML)
		return
	}
	routingMu.RLock()
	svcState, ok := servicesMap[svc]
	routingMu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/openapi.yaml"
	r2.URL.RawPath = ""
	// Strip all auth/identity headers so a client cannot forge conductor's internal
	// headers to influence backend behaviour on the /openapi.yaml proxy path.
	r2.Header.Del("Authorization")
	r2.Header.Del("X-User-ID")
	r2.Header.Del("X-Conductor-Token")
	r2.Header.Del("X-Conductor-Timestamp")
	svcState.proxy.ServeHTTP(w, r2)
}

func generateOpenAPISpec() openAPISpec {
	routingMu.RLock()
	endpoints := make([]endpointEntry, len(endpointsList))
	copy(endpoints, endpointsList)
	services := make(map[string]serviceState, len(servicesMap))
	for k, v := range servicesMap {
		services[k] = v
	}
	routingMu.RUnlock()

	paths := make(map[string]openAPIPathItem)
	tagDescs := map[string]string{}

	for _, ep := range endpoints {
		method := strings.ToLower(ep.method)

		var params []openAPIParameter
		for _, name := range ep.paramNames {
			params = append(params, openAPIParameter{
				Name:     name,
				In:       "path",
				Required: true,
				Schema:   openAPISchema{Type: "string"},
			})
		}

		var security []map[string][]string
		if ep.public {
			security = []map[string][]string{}
		} else {
			security = []map[string][]string{{"bearerAuth": {}}}
		}

		op := openAPIOperation{
			Tags:        []string{ep.serviceName},
			Summary:     ep.action,
			OperationID: ep.serviceName + "_" + ep.action,
			Parameters:  params,
			Security:    security,
			Responses: map[string]openAPIResponse{
				"200": {Description: "Success"},
				"401": {Description: "Unauthorized"},
				"403": {Description: "Forbidden"},
				"404": {Description: "Not found"},
			},
		}

		// Use the original registry path directly — conductor's full-path matching
		// (step 2) handles these without any service prefix.
		specPath := ep.originalPath
		if _, ok := paths[specPath]; !ok {
			paths[specPath] = make(openAPIPathItem)
		}
		paths[specPath][method] = op

		if svc, ok := services[ep.serviceName]; ok {
			tagDescs[ep.serviceName] = svc.description
		}
	}

	tagNames := make([]string, 0, len(tagDescs))
	for name := range tagDescs {
		tagNames = append(tagNames, name)
	}
	sort.Strings(tagNames)

	tags := make([]openAPITag, 0, len(tagNames))
	for _, name := range tagNames {
		tags = append(tags, openAPITag{Name: name, Description: tagDescs[name]})
	}

	return openAPISpec{
		OpenAPI: "3.0.3",
		Info: openAPIInfo{
			Title:       "CodeArmory API",
			Description: "All endpoints routed through the CodeArmory API gateway. Endpoints marked with no security are public and do not require authentication.",
			Version:     "1.0.0",
		},
		Tags:  tags,
		Paths: paths,
		Components: openAPIComponents{
			SecuritySchemes: map[string]openAPISecurityScheme{
				"bearerAuth": {
					Type:         "http",
					Scheme:       "bearer",
					BearerFormat: "JWT",
				},
			},
		},
	}
}

func handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(generateOpenAPISpec())
}

// ── OpenAPI struct types ──────────────────────────────────────────────────────

type openAPISpec struct {
	OpenAPI    string                     `json:"openapi"`
	Info       openAPIInfo                `json:"info"`
	Tags       []openAPITag               `json:"tags,omitempty"`
	Paths      map[string]openAPIPathItem `json:"paths"`
	Components openAPIComponents          `json:"components"`
}

type openAPIInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

type openAPITag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// openAPIPathItem maps HTTP method (lowercase) → operation.
type openAPIPathItem map[string]openAPIOperation

type openAPIOperation struct {
	Tags        []string                   `json:"tags,omitempty"`
	Summary     string                     `json:"summary,omitempty"`
	OperationID string                     `json:"operationId,omitempty"`
	Parameters  []openAPIParameter         `json:"parameters,omitempty"`
	Security    []map[string][]string      `json:"security"`
	Responses   map[string]openAPIResponse `json:"responses"`
}

type openAPIParameter struct {
	Name     string        `json:"name"`
	In       string        `json:"in"`
	Required bool          `json:"required"`
	Schema   openAPISchema `json:"schema"`
}

type openAPISchema struct {
	Type string `json:"type"`
}

type openAPIResponse struct {
	Description string `json:"description"`
}

type openAPIComponents struct {
	SecuritySchemes map[string]openAPISecurityScheme `json:"securitySchemes"`
}

type openAPISecurityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// ── HTML templates ────────────────────────────────────────────────────────────

const docsLandingHTML = `<!DOCTYPE html>
<html>
<head>
  <title>CodeArmory API Docs</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 640px; margin: 60px auto; padding: 0 24px; color: #111; }
    h1 { font-size: 1.4rem; margin-bottom: 4px; }
    p  { color: #555; margin-top: 0; margin-bottom: 24px; font-size: 0.9rem; }
    ul { list-style: none; padding: 0; margin: 0; }
    li { padding: 10px 0; border-bottom: 1px solid #eee; font-size: 0.95rem; }
    li:last-child { border-bottom: none; }
    a  { color: #2563eb; text-decoration: none; font-weight: 600; }
    a:hover { text-decoration: underline; }
    span { color: #555; }
  </style>
</head>
<body>
  <h1>CodeArmory API Documentation</h1>
  <p>Select a service to browse its endpoints.</p>
  <ul>
    %s
  </ul>
</body>
</html>`

const serviceSwaggerHTML = `<!DOCTYPE html>
<html>
<head>
  <title>CodeArmory API</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" type="text/css" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
window.onload = function() {
  SwaggerUIBundle({
    url: "%s",
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true,
  })
}
</script>
</body>
</html>`
