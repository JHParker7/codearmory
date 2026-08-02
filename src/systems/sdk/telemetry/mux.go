package telemetry

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Mux wraps http.ServeMux and updates the active OTel span name to the matched
// route pattern on each request, giving traces operation-level names like
// "GET /services/{id}" instead of the generic service name set by otelhttp.
type Mux struct{ *http.ServeMux }

// NewMux returns a new Mux, replacing http.NewServeMux.
func NewMux() *Mux { return &Mux{http.NewServeMux()} }

func (m *Mux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.ServeMux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		trace.SpanFromContext(r.Context()).SetName(RouteSpanName(r))
		h(w, r)
	})
}

func (m *Mux) Handle(pattern string, h http.Handler) {
	m.ServeMux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace.SpanFromContext(r.Context()).SetName(RouteSpanName(r))
		h.ServeHTTP(w, r)
	}))
}

// RouteSpanName derives a span name from the matched route. Go 1.22 patterns
// registered with a method prefix ("GET /users/{id}") are returned verbatim;
// bare patterns ("/{path...}") get the request method prepended.
func RouteSpanName(r *http.Request) string {
	for _, m := range []string{"GET ", "POST ", "PUT ", "DELETE ", "PATCH ", "HEAD ", "OPTIONS "} {
		if strings.HasPrefix(r.Pattern, m) {
			return r.Pattern
		}
	}
	return r.Method + " " + r.Pattern
}
