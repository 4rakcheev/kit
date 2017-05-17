package sd

import (
	"io"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/log"
)

// endpointCache collects the most recent set of instances from a service discovery
// system, creates endpoints for them using a factory function, and makes
// them available to consumers.
type endpointCache struct {
	options            endpointerOptions
	mtx                sync.RWMutex
	factory            Factory
	cache              map[string]endpointCloser
	err                error
	endpoints          []endpoint.Endpoint
	logger             log.Logger
	invalidateDeadline time.Time
}

type endpointCloser struct {
	endpoint.Endpoint
	io.Closer
}

// newEndpointCache returns a new, empty endpointCache.
func newEndpointCache(factory Factory, logger log.Logger, options endpointerOptions) *endpointCache {
	return &endpointCache{
		options: options,
		factory: factory,
		cache:   map[string]endpointCloser{},
		logger:  logger,
	}
}

// Update should be invoked by clients with a complete set of current instance
// strings whenever that set changes. The cache manufactures new endpoints via
// the factory, closes old endpoints when they disappear, and persists existing
// endpoints if they survive through an update.
func (c *endpointCache) Update(event Event) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	if event.Err == nil {
		c.updateCache(event.Instances)
		c.invalidateDeadline = time.Time{}
		c.err = nil
	}

	c.logger.Log("err", event.Err)

	if c.options.invalidateOnErrorTimeout == nil {
		// keep returning the last known endpoints on error
		return
	}

	c.err = event.Err

	if !c.invalidateDeadline.IsZero() {
		// aleady in the error state, do nothing
		return
	}
	// set new deadline to invalidate Endpoints unless non-error Event is received
	c.invalidateDeadline = time.Now().Add(*c.options.invalidateOnErrorTimeout)
	return
}

func (c *endpointCache) updateCache(instances []string) {
	// Deterministic order (for later).
	sort.Strings(instances)

	// Produce the current set of services.
	cache := make(map[string]endpointCloser, len(instances))
	for _, instance := range instances {
		// If it already exists, just copy it over.
		if sc, ok := c.cache[instance]; ok {
			cache[instance] = sc
			delete(c.cache, instance)
			continue
		}

		// If it doesn't exist, create it.
		service, closer, err := c.factory(instance)
		if err != nil {
			c.logger.Log("instance", instance, "err", err)
			continue
		}
		cache[instance] = endpointCloser{service, closer}
	}

	// Close any leftover endpoints.
	for _, sc := range c.cache {
		if sc.Closer != nil {
			sc.Closer.Close()
		}
	}

	// Populate the slice of endpoints.
	endpoints := make([]endpoint.Endpoint, 0, len(cache))
	for _, instance := range instances {
		// A bad factory may mean an instance is not present.
		if _, ok := cache[instance]; !ok {
			continue
		}
		endpoints = append(endpoints, cache[instance].Endpoint)
	}

	// Swap and trigger GC for old copies.
	c.endpoints = endpoints
	c.cache = cache
}

// Endpoints yields the current set of (presumably identical) endpoints, ordered
// lexicographically by the corresponding instance string.
func (c *endpointCache) Endpoints() ([]endpoint.Endpoint, error) {
	c.mtx.RLock()

	if c.err == nil || time.Now().Before(c.invalidateDeadline) {
		defer c.mtx.RUnlock()
		return c.endpoints, nil
	}

	c.mtx.RUnlock()
	c.mtx.Lock()
	defer c.mtx.Unlock()

	c.updateCache(nil) // close any remaining active endpoints

	return nil, c.err
}
