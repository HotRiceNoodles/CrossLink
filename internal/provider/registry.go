package provider

import "sync"

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

func (r *Registry) Register(name string, p Provider) {
	r.mu.Lock()
	r.providers[name] = p
	r.mu.Unlock()
}

func (r *Registry) Remove(name string) {
	r.mu.Lock()
	delete(r.providers, name)
	r.mu.Unlock()
}

func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	p, ok := r.providers[name]
	r.mu.RUnlock()
	return p, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.providers))
	for k := range r.providers {
		names = append(names, k)
	}
	r.mu.RUnlock()
	return names
}
