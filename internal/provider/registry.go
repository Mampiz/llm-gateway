package provider

import (
	"fmt"
	"sort"
	"strings"
)

// Registry decides which Provider serves a given model, and is the seam the
// automatic fallback of phase 5 will hang from.
//
// It deliberately knows no vendor names: the prefixes are supplied by the
// caller at wiring time, so adding a sixth provider never means editing this
// file.
type Registry struct {
	byName map[string]Provider
	routes []route
	def    Provider
}

type route struct {
	prefix string
	p      Provider
}

// NewRegistry returns an empty registry. Until SetDefault is called, a model
// matching no route is an error rather than being sent somewhere arbitrary.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Provider)}
}

// SetDefault names the provider that serves models matching no prefix. It must
// already be registered.
func (r *Registry) SetDefault(name string) error {
	p, ok := r.byName[name]
	if !ok {
		return fmt.Errorf("cannot default to provider %q: it is not registered", name)
	}
	r.def = p
	return nil
}

// Register adds p and routes every model whose name starts with one of the
// given prefixes to it. Longer prefixes are matched first, so a specific rule
// always beats a general one regardless of registration order.
func (r *Registry) Register(p Provider, prefixes ...string) {
	r.byName[p.Name()] = p
	for _, prefix := range prefixes {
		r.routes = append(r.routes, route{prefix: prefix, p: p})
	}
	sort.SliceStable(r.routes, func(i, j int) bool {
		return len(r.routes[i].prefix) > len(r.routes[j].prefix)
	})
}

// For resolves the provider that should serve model.
func (r *Registry) For(model string) (Provider, error) {
	for _, rt := range r.routes {
		if strings.HasPrefix(model, rt.prefix) {
			return rt.p, nil
		}
	}
	if r.def != nil {
		return r.def, nil
	}
	return nil, fmt.Errorf("no provider is registered for model %q", model)
}

// ByName returns a provider by its own name, which is how phase 5 will pick an
// explicit fallback target.
func (r *Registry) ByName(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names lists every registered provider, sorted, for /healthz and logging.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
