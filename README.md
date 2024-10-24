# Replay
Configurable http caching middleware for Go servers.   
Improve API throughput, reduce latency, and save on third-party resources.

## Features

- **Eviction Policy**: FIFO, LRU
- **Cache Size**: Based on number of entries, or memory usage
- **Cache Filters**: Filter on URL, method, or header fields
- **TTL + Max TTL**: Cache expiration and renewal
- **Logger**: Pass any logger package
- **Metrics**: Track cache hits, misses, and evictions

## Example

```go
import (
	"log"
	"net/http"
	"os"
	"time"
	"github.com/ztkent/replay"
	"github.com/go-chi/chi/v5"
)

func main() {
	r := chi.NewRouter()
	c := replay.NewCache(
		replay.WithMaxSize(100),
		replay.WithMaxMemory(100*1024*1024),
		replay.WithCacheFilters([]string{"URL", "Method"}),
		replay.WithCacheFailures(false),
		replay.WithEvictionPolicy("LRU"),
		replay.WithTTL(5*time.Minute),
		replay.WithMaxTTL(30*time.Minute),
		replay.WithLogger(log.New(os.Stdout, "replay: ", log.LstdFlags)),
	)

	// Apply the middleware to the entire router
	r.Use(c.Middleware)
	// Or apply it to a specific endpoint
	r.Get("/test-endpoint", c.MiddlewareFunc(testHandlerFunc()))
	http.ListenAndServe(os.Getenv("SERVER_PORT"), r)
}
```

## Options

The cache configured with the following options:

- `WithMaxSize(maxSize int)`: Set the maximum number of entries in the cache.
- `WithMaxMemory(maxMemory uint64)`: Set the maximum memory usage of the cache.
- `WithEvictionPolicy(evictionPolicy string)`: Set the eviction policy for the cache [FIFO, LRU].
- `WithEvictionTimer(evictionTimer time.Duration)`: Set the time between cache eviction checks.
- `WithTTL(ttl time.Duration)`: Set the time a cache entry can live without being accessed.
- `WithMaxTTL(maxTtl time.Duration)`: Set the maximum time a cache entry can live, including renewals.
- `WithCacheFilters(cacheFilters []string)`: Set the cache filters to use for generating cache keys.
- `WithCacheFailures(cacheFailures bool)`: Set whether to cache failed requests.
- `WithLogger(l *log.Logger)`: Set the logger to use for cache logging.

## Metrics

You can access cache metrics to monitor cache performance:

```go
metrics := c.Metrics()
```

Fields Available:
- `Hits`: Total number of cache hits.
- `Misses`: Total number of cache misses.
- `Evictions`: Total umber of cache evictions.
- `CurrentSize`: Number of entries in the cache.
- `CurrentMemory`: Current memory usage of the cache.
