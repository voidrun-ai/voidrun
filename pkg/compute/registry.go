package compute

import (
	"fmt"
	"sync"
)

var (
	mu        sync.RWMutex
	factories = map[string]func() Hypervisor{}
)

// Register adds a hypervisor factory. Panics if name is duplicate.
func Register(name string, factory func() Hypervisor) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("compute: duplicate hypervisor registration %q", name))
	}
	factories[name] = factory
}

// Get returns a new hypervisor instance for name.
func Get(name string) (Hypervisor, error) {
	mu.RLock()
	factory, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("compute: hypervisor %q not registered", name)
	}
	return factory(), nil
}

// List returns registered hypervisor names.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for name := range factories {
		out = append(out, name)
	}
	return out
}

// ResolveType picks sandbox hypervisor or host default.
func ResolveType(sandboxHV, defaultHV string) Type {
	if sandboxHV != "" {
		return Type(sandboxHV)
	}
	if defaultHV != "" {
		return Type(defaultHV)
	}
	return TypeCloudHypervisor
}
