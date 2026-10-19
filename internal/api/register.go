package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// PathPrefix is the prefix of every API operation path.
const PathPrefix = "/v1/"

// MCPPath is where the Model Context Protocol endpoint is served.
const MCPPath = "/mcp"

var operationIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// Register adds an operation that requires the bearer token. It panics if op
// violates the package conventions (see the package documentation); like
// huma.Register, registration errors are programming errors caught at startup.
func Register[I, O any](s *Server, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	mustValidate(s, &op)
	if !slices.Contains(op.Errors, http.StatusUnauthorized) {
		op.Errors = append(op.Errors, http.StatusUnauthorized)
	}
	huma.Register(s.api, op, handler)
}

// RegisterPublic adds an operation that is reachable without the bearer
// token. Use it sparingly: only for endpoints that reveal nothing sensitive.
func RegisterPublic[I, O any](s *Server, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	mustValidate(s, &op)
	// An explicit empty requirement overrides the document-wide bearer scheme.
	op.Security = []map[string][]string{}
	huma.Register(s.api, op, handler)
	s.mu.Lock()
	s.public[routePattern(op.Method, op.Path)] = struct{}{}
	s.mu.Unlock()
}

func mustValidate(s *Server, op *huma.Operation) {
	if err := validateOperation(s.api.OpenAPI(), op); err != nil {
		panic(err)
	}
}

func validateOperation(doc *huma.OpenAPI, op *huma.Operation) error {
	if !operationIDPattern.MatchString(op.OperationID) {
		return fmt.Errorf("api: operation %s %s: operationID %q must be kebab-case", op.Method, op.Path, op.OperationID)
	}
	if len(op.Tags) == 0 {
		return fmt.Errorf("api: operation %q: at least one tag is required", op.OperationID)
	}
	if !strings.HasPrefix(op.Path, PathPrefix) {
		return fmt.Errorf("api: operation %q: path %q must start with %q", op.OperationID, op.Path, PathPrefix)
	}
	if op.Method == "" {
		return fmt.Errorf("api: operation %q: method is required", op.OperationID)
	}
	for _, item := range doc.Paths {
		for _, existing := range pathOperations(item) {
			if existing != nil && existing.OperationID == op.OperationID {
				return fmt.Errorf("api: duplicate operationID %q", op.OperationID)
			}
		}
	}
	return nil
}

func pathOperations(p *huma.PathItem) []*huma.Operation {
	return []*huma.Operation{p.Get, p.Put, p.Post, p.Delete, p.Options, p.Head, p.Patch, p.Trace}
}

// routePattern returns the http.ServeMux pattern humago registers for an
// operation.
func routePattern(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

func (s *Server) isPublicPattern(pattern string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.public[pattern]
	return ok
}
