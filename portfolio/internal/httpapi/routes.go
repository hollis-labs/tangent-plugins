// Package httpapi adapts the internal shadow registry to source-only plugin
// HTTP requests. It supplies no production caller verifier or live data root.
package httpapi

import (
	"net/http"

	"github.com/hollis-labs/tangent-plugins/portfolio/internal/operations"
	"github.com/hollis-labs/tangent/pkg/plugin"
)

// Prefix owns literal operation paths, not legacy resource URL patterns.
const Prefix = "/api/plugins/portfolio/operations/"

// Route keeps one detached registry declaration with its host route binding.
type Route struct {
	Declaration plugin.RouteDecl
	Operation   operations.Operation
}

// Routes derives transport declarations from the domain registry. Administrative
// migrate has no HTTP grant. Participant capabilities are host gates, not grants
// to the portfolio service.
func Routes() []Route {
	var routes []Route
	for _, op := range operations.Registry() {
		if op.Name == "migrate" {
			continue
		}
		capability := "view"
		if op.Write {
			capability = "draft"
		}
		switch op.Name {
		case "decide", "defer", "reopen":
			capability = "resolve"
		}
		routes = append(routes, Route{plugin.RouteDecl{Method: http.MethodPost, Path: Prefix + op.Name, Capability: capability}, op})
	}
	return routes
}
