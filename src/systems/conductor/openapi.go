package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

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
	Tags        []string              `json:"tags,omitempty"`
	Summary     string                `json:"summary,omitempty"`
	OperationID string                `json:"operationId,omitempty"`
	Parameters  []openAPIParameter    `json:"parameters,omitempty"`
	Security    []map[string][]string `json:"security"`
	Responses   map[string]openAPIResponse `json:"responses"`
}

type openAPIParameter struct {
	Name     string       `json:"name"`
	In       string       `json:"in"`
	Required bool         `json:"required"`
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
	refreshServiceCache(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(generateOpenAPISpec())
}

func handleDocs(w http.ResponseWriter, r *http.Request) {
	refreshServiceCache(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(swaggerUIHTML))
}

const swaggerUIHTML = `<!DOCTYPE html>
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
    url: "/openapi.json",
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true,
  })
}
</script>
</body>
</html>`
