package target

import (
	"fmt"
	"sort"
	"strings"
)

// Registry maps a target kind to its Backend. Registration is explicit — there
// is no init()-time side-effect magic — so the set of supported backends is
// whatever a caller wired up, and unknown kinds fail with a clear diagnostic.
type Registry struct {
	backends map[string]Backend
}

func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]Backend)}
}

// Register adds a backend. Registering two backends for the same kind is a
// programming error and panics, since it would silently shadow one.
func (r *Registry) Register(backend Backend) {
	kind := backend.Kind()
	if _, exists := r.backends[kind]; exists {
		panic(fmt.Sprintf("target: backend for kind %q already registered", kind))
	}
	r.backends[kind] = backend
}

// Compile looks up the backend for kind and compiles the input. An unregistered
// kind yields an UnknownTargetError listing the supported kinds.
func (r *Registry) Compile(kind string, input CompileInput) (*Bundle, error) {
	backend, ok := r.backends[kind]
	if !ok {
		return nil, &UnknownTargetError{Kind: kind, Supported: r.Kinds()}
	}
	return backend.Compile(input)
}

// Kinds returns the registered target kinds in sorted order.
func (r *Registry) Kinds() []string {
	kinds := make([]string, 0, len(r.backends))
	for kind := range r.backends {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// UnknownTargetError is returned when a caller asks for a target kind that is
// not a registered bundle backend.
type UnknownTargetError struct {
	Kind      string
	Supported []string
}

func (e *UnknownTargetError) Error() string {
	if len(e.Supported) == 0 {
		return fmt.Sprintf("no bundle backend registered for target %q; no backends are registered", e.Kind)
	}
	return fmt.Sprintf("no bundle backend registered for target %q; supported bundle targets: %s", e.Kind, strings.Join(e.Supported, ", "))
}
