package main

import (
	"container/list"
	"sync"
	"time"
)

// cached is one in-memory answer: a small body, or an upstream "not
// found" remembered for a short while.
type cached struct {
	status      int
	contentType string
	body        []byte
	// etag and fetched belong to a mutable document that is revalidated (an
	// npm packument): the validator upstream gave, and when it was last
	// confirmed current.
	etag    string
	fetched time.Time
}

// lru is a byte-bounded least-recently-used map with an optional expiry
// per entry. Memory only: losing it costs S3 reads, never correctness.
type lru struct {
	mu  sync.Mutex
	max int64
	// entryMax is the largest entry kept; 0 means an eighth of max.
	entryMax int64
	size     int64
	ll       *list.List
	items    map[string]*list.Element
	now      func() time.Time
}

type lruEntry struct {
	key     string
	val     cached
	size    int64
	expires time.Time // zero: never
}

func newLRU(maxBytes int64, now func() time.Time) *lru {
	return &lru{max: maxBytes, ll: list.New(), items: map[string]*list.Element{}, now: now}
}

func (c *lru) Get(key string) (cached, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return cached{}, false
	}
	e := el.Value.(*lruEntry)
	if !e.expires.IsZero() && !c.now().Before(e.expires) {
		c.remove(el)
		return cached{}, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

// Put remembers val under key for ttl (0: until evicted). An entry larger
// than an eighth of the budget (or entryMax) is not kept: one big object must
// not empty the cache of everything small.
func (c *lru) Put(key string, val cached, ttl time.Duration) {
	size := int64(len(key)+len(val.body)) + 64
	limit := c.max / 8
	if c.entryMax > 0 {
		limit = c.entryMax
	}
	if c.max <= 0 || size > limit {
		return
	}
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.remove(el)
	}
	c.items[key] = c.ll.PushFront(&lruEntry{key: key, val: val, size: size, expires: exp})
	c.size += size
	for c.size > c.max {
		c.remove(c.ll.Back())
	}
}

func (c *lru) remove(el *list.Element) {
	e := el.Value.(*lruEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.size -= e.size
}
