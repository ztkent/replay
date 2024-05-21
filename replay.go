package replay

import (
	"bytes"
	"container/list"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

/*
Configurable http caching middleware for Go servers.
*/

// Cache stores the cache configuration and data
type Cache struct {
	maxSize        int            // Maximum number of entries in the cache
	maxMemory      uint64         // Maximum memory usage for the cache
	evictionPolicy EvictionPolicy // Cache eviction policy (LRU, FIFO)
	evictionTimer  time.Duration  // Time interval for checking expired entries
	ttl            time.Duration  // Time-To-Live for cache entries
	maxTtl         time.Duration  // Maximum lifespan for cache entries
	cacheList      *list.List     // Linked list containing the cache data
	cacheFilters   []string       // Filters used for cache key generation
	mut            sync.Mutex     // Mutex for synchronizing access to the cache
	l              *log.Logger    // Logger for cache logging
	metrics        *CacheMetrics  // Metrics for cache performance
}

// CacheEntry stores a single cache item
type CacheEntry struct {
	key          string
	value        *CacheResponse
	size         uint64
	created      time.Time
	lastAccessed time.Time
}

// CacheResponse stores the response info to be cached
type CacheResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// CacheMetrics stores the cache metrics
type CacheMetrics struct {
	Hits          int
	Misses        int
	Evictions     int
	CurrentSize   int
	CurrentMemory uint64
}

const (
	DefaultMaxSize        = 25
	DefaultMaxMemory      = 100 * 1024 * 1024
	DefaultEvictionPolicy = LRU
	DefaultEvictionTimer  = 1 * time.Minute
	DefaultTTL            = 5 * time.Minute
	DefaultMaxTTL         = 10 * time.Minute
	DefaultFilter         = "URL"
)

type EvictionPolicy string

const (
	LRU  EvictionPolicy = "LRU"
	FIFO EvictionPolicy = "FIFO"
)

type CacheOption func(*Cache)

// NewCache initializes a new instance of Cache with given options
func NewCache(options ...CacheOption) *Cache {
	c := &Cache{
		maxSize:        DefaultMaxSize,
		maxMemory:      DefaultMaxMemory,
		evictionPolicy: DefaultEvictionPolicy,
		ttl:            DefaultTTL,
		maxTtl:         DefaultMaxTTL,
		cacheList:      list.New(),
		cacheFilters:   []string{DefaultFilter},
		evictionTimer:  DefaultEvictionTimer,
		l:              log.New(io.Discard, "", 0),
		metrics:        &CacheMetrics{},
	}
	for _, option := range options {
		option(c)
	}
	go c.clearExpiredEntries()
	return c
}

// Set the maximum number of entries in the cache
func WithMaxSize(maxSize int) CacheOption {
	return func(c *Cache) {
		if maxSize != 0 {
			c.maxSize = maxSize
		}
	}
}

// Set the maximum memory usage of the cache
func WithMaxMemory(maxMemory uint64) CacheOption {
	return func(c *Cache) {
		if maxMemory != 0 {
			c.maxMemory = maxMemory
		}
	}
}

// Set the eviction policy for the cache [FIFO, LRU]
func WithEvictionPolicy(evictionPolicy string) CacheOption {
	return func(c *Cache) {
		if evictionPolicy != "" {
			c.evictionPolicy = EvictionPolicy(evictionPolicy)
		}
	}
}

// Set the time between cache eviction checks
func WithEvictionTimer(evictionTimer time.Duration) CacheOption {
	return func(c *Cache) {
		if evictionTimer > time.Minute {
			c.evictionTimer = evictionTimer
		}
	}
}

// Set the time a cache entry can live without being accessed
func WithTTL(ttl time.Duration) CacheOption {
	return func(c *Cache) {
		if ttl > 0 {
			c.ttl = ttl
		}
	}
}

// Set the maximum time a cache entry can live, including renewals
func WithMaxTTL(maxTtl time.Duration) CacheOption {
	return func(c *Cache) {
		if maxTtl > c.ttl {
			c.maxTtl = maxTtl
		}
	}
}

// Set the cache filters to use for generating cache keys
func WithCacheFilters(cacheFilters []string) CacheOption {
	return func(c *Cache) {
		if len(cacheFilters) != 0 {
			c.cacheFilters = cacheFilters
		}
	}
}

// Set the logger to use for cache logging
func WithLogger(l *log.Logger) CacheOption {
	return func(c *Cache) {
		c.l = l
	}
}

// Middleware function to intercept all HTTP requests on a handler.
func (c *Cache) Middleware(next http.Handler) http.Handler {
	return c.middleware(next.(http.HandlerFunc))
}

// Middleware function to intercept HTTP requests to a given route
func (c *Cache) MiddlewareFunc(next http.HandlerFunc) http.HandlerFunc {
	return c.middleware(next)
}

func (c *Cache) middleware(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		key := c.generateKey(r)
		wasCached := false
		defer func() {
			if wasCached {
				c.metrics.Hits++
			} else {
				c.metrics.Misses++
			}
			c.l.Printf("Request: %s, Cached: %v, Duration: %v", key, wasCached, time.Since(start))
		}()

		c.mut.Lock()
		defer c.mut.Unlock()
		if ele, found := c.findKey(key); found {
			entry := ele.Value.(*CacheEntry)
			if entry.lastAccessed.Add(c.ttl).After(time.Now()) {
				// valid, not expired
				c.l.Printf("Serving from cache: %s", key)
				c.serveFromCache(w, entry)
				wasCached = true
				return
			}
			// accessed expired item, remove the item
			c.l.Printf("Cache entry expired: %s, removing from cache", key)
			c.cacheList.Remove(ele)
		} else {
			c.l.Printf("Cache miss: %s", key)
		}
		// not in cache or expired, serve then cache the response
		wr := &writerRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wr, r)
		if wr.statusCode == http.StatusOK {
			// cache only the successful responses to prevent chaos
			go c.addToCache(key, wr.Result())
		}
	})
}

func (c *Cache) Metrics() *CacheMetrics {
	c.mut.Lock()
	defer c.mut.Unlock()
	return &CacheMetrics{
		Hits:          c.metrics.Hits,
		Misses:        c.metrics.Misses,
		Evictions:     c.metrics.Evictions,
		CurrentSize:   c.cacheList.Len(),
		CurrentMemory: calculateCacheMemory(c),
	}
}

// clear any expired cache entries
func (c *Cache) clearExpiredEntries() {
	timer := time.NewTicker(c.evictionTimer)
	for range timer.C {
		func() {
			c.mut.Lock()
			defer c.mut.Unlock()
			for ele := c.cacheList.Front(); ele != nil; ele = ele.Next() {
				entry := ele.Value.(*CacheEntry)
				if entry.lastAccessed.Add(c.ttl).Before(time.Now()) ||
					entry.created.Add(c.maxTtl).Before(time.Now()) {
					c.l.Printf("Cache entry expired: %s, removing from cache", entry.key)
					c.cacheList.Remove(ele)
				}
			}
		}()
	}
}

// generate cache key based on request + selected filters
func (c *Cache) generateKey(r *http.Request) string {
	keyParts := []string{r.URL.String()}
	for _, filter := range c.cacheFilters {
		switch filter {
		case "Method":
			keyParts = append(keyParts, r.Method)
		case "Header":
			for k, v := range r.Header {
				for _, hv := range v {
					keyParts = append(keyParts, fmt.Sprintf("%s=%s", k, hv))
				}
			}
		}
	}
	return strings.Join(keyParts, "|")
}

// find a cache entry in the list based on key
func (c *Cache) findKey(key string) (*list.Element, bool) {
	for ele := c.cacheList.Front(); ele != nil; ele = ele.Next() {
		entry := ele.Value.(*CacheEntry)
		if entry.key == key {
			return ele, true
		}
	}
	return nil, false
}

// serve the response from cache
func (c *Cache) serveFromCache(w http.ResponseWriter, entry *CacheEntry) {
	entry.lastAccessed = time.Now()
	for k, v := range entry.value.Header {
		for _, hv := range v {
			w.Header().Add(k, hv)
		}
	}
	w.WriteHeader(entry.value.StatusCode)
	w.Write(entry.value.Body)
}

// add a new response to the cache
func (c *Cache) addToCache(key string, resp *http.Response) {
	cacheResp, cacheSize := cloneResponse(resp)
	entry := &CacheEntry{
		key:          key,
		value:        cacheResp,
		size:         cacheSize,
		created:      time.Now(),
		lastAccessed: time.Now(),
	}
	if entry.size > c.maxMemory {
		c.l.Printf("Response too large to cache: %s", key)
		return
	}

	c.mut.Lock()
	defer c.mut.Unlock()
	c.cacheList.PushFront(entry)
	c.checkEvictions()
}

// stay in bounds of cache size and memory limits
func (c *Cache) checkEvictions() {
	// Check the cache size
	for c.cacheList.Len() >= c.maxSize {
		c.l.Printf("Cache is full, evicting an item")
		c.evict()
	}

	// Handle memory limit
	for calculateCacheMemory(c) > c.maxMemory && c.cacheList.Len() != 0 {
		c.l.Printf("Cache exceeds max memory, evicting an item")
		c.evict()
	}
}

func calculateCacheMemory(c *Cache) uint64 {
	var currentMemory uint64
	for ele := c.cacheList.Front(); ele != nil; ele = ele.Next() {
		entry := ele.Value.(*CacheEntry)
		currentMemory += entry.size
	}
	return currentMemory
}

// evict from cache based on policy
func (c *Cache) evict() {
	var ele *list.Element
	if c.evictionPolicy == "FIFO" {
		ele = c.cacheList.Back()
	} else if c.evictionPolicy == "LRU" {
		var oldest time.Time
		for e := c.cacheList.Front(); e != nil; e = e.Next() {
			entry := e.Value.(*CacheEntry)
			if oldest.IsZero() || entry.lastAccessed.Before(oldest) {
				oldest = entry.lastAccessed
				ele = e
			}
		}
	}
	if ele != nil {
		entry := ele.Value.(*CacheEntry)
		c.l.Printf("Evicting: %v", entry.key)
		c.cacheList.Remove(ele)
		c.metrics.Evictions++
	}
}

// copy the response for caching
func cloneResponse(resp *http.Response) (*CacheResponse, uint64) {
	var buf bytes.Buffer
	if resp.Body != nil {
		_, err := io.Copy(&buf, resp.Body)
		if err != nil {
			log.Printf("Error copying response body: %v", err)
		}
		resp.Body.Close()
	}
	cacheResp := &CacheResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       buf.Bytes(),
	}
	return cacheResp, sizeOfResponse(cacheResp)
}

// calculate the size of the cache object
func sizeOfResponse(resp *CacheResponse) uint64 {
	headerSize := uint64(0)
	for k, v := range resp.Header {
		headerSize += uint64(len(k) + len(v[0]))
	}
	return uint64(len(resp.Body)) + headerSize
}

// Capture our response on the fly, lets us cache it.
type writerRecorder struct {
	http.ResponseWriter
	statusCode int
	headers    http.Header
	body       io.ReadWriter
}

func (wr *writerRecorder) WriteHeader(statusCode int) {
	wr.statusCode = statusCode
	wr.ResponseWriter.WriteHeader(statusCode)
}

func (wr *writerRecorder) Write(body []byte) (int, error) {
	if wr.body == nil {
		wr.body = new(bytes.Buffer)
	}
	wr.body.Write(body)
	return wr.ResponseWriter.Write(body)
}

func (wr *writerRecorder) Header() http.Header {
	if wr.headers == nil {
		wr.headers = make(http.Header)
	}
	return wr.headers
}

func (wr *writerRecorder) Result() *http.Response {
	return &http.Response{
		StatusCode:    wr.statusCode,
		Header:        wr.headers,
		Body:          io.NopCloser(bytes.NewBuffer(wr.body.(*bytes.Buffer).Bytes())),
		ContentLength: int64(wr.body.(*bytes.Buffer).Len()),
	}
}
