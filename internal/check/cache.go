package check

import (
	"sync"

	"github.com/APD007/zanzo/internal/storage"
)

// Cache stores decided subproblems across requests.
//
// Caching an authorization decision is dangerous in a way that caching a
// product listing is not: a stale hit does not show someone an old price, it
// shows them a document they were just removed from. Zanzibar's answer is that
// the cache key includes the revision the answer was computed at, so an entry
// can never outlive the state that produced it. Nothing is ever invalidated;
// entries for old revisions simply stop being asked for.
type Cache interface {
	Get(key Key) (allowed bool, ok bool)
	Set(key Key, allowed bool)
	Len() int
}

// Key identifies a subproblem. Revision is part of the identity, which is the
// entire trick.
type Key struct {
	Object   storage.Object
	Relation string
	Subject  string
	Revision storage.Revision
}

// RevisionKeyed is the correct cache: entries are scoped to the revision they
// were computed at.
type RevisionKeyed struct {
	mu      sync.RWMutex
	entries map[Key]bool
}

func NewRevisionKeyed() *RevisionKeyed {
	return &RevisionKeyed{entries: map[Key]bool{}}
}

func (c *RevisionKeyed) Get(k Key) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.entries[k]
	return v, ok
}

func (c *RevisionKeyed) Set(k Key, allowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[k] = allowed
}

func (c *RevisionKeyed) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// NaiveCache drops the revision from the key -- the obvious implementation, and
// the one that produces the new-enemy problem. It exists so the regression test
// can demonstrate the bug rather than merely assert its absence.
//
// Do not use it for anything.
type NaiveCache struct {
	mu      sync.RWMutex
	entries map[Key]bool
}

func NewNaiveCache() *NaiveCache {
	return &NaiveCache{entries: map[Key]bool{}}
}

func (c *NaiveCache) strip(k Key) Key { k.Revision = 0; return k }

func (c *NaiveCache) Get(k Key) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.entries[c.strip(k)]
	return v, ok
}

func (c *NaiveCache) Set(k Key, allowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[c.strip(k)] = allowed
}

func (c *NaiveCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
