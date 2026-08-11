// Package storage holds relationship tuples.
//
// A tuple is (object, relation, subject). The subject is either a concrete
// user or a *userset* -- a reference to every member of some relation on
// another object, written group:eng#member. Userset subjects are what let a
// group be granted access once instead of once per member.
package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Object is a typed entity: doc:readme, folder:eng, user:ayush.
type Object struct {
	Type string
	ID   string
}

func (o Object) String() string { return o.Type + ":" + o.ID }

// Subject is either a concrete object (Relation == "") or a userset
// (Relation != ""), e.g. {group, eng, member} for group:eng#member.
type Subject struct {
	Object   Object
	Relation string
}

func (s Subject) IsUserset() bool { return s.Relation != "" }

func (s Subject) String() string {
	if s.IsUserset() {
		return s.Object.String() + "#" + s.Relation
	}
	return s.Object.String()
}

type Tuple struct {
	Object   Object
	Relation string
	Subject  Subject
}

// Revision identifies a point in the store's history. Every write advances it.
//
// This is what makes aggressive caching safe: a cached subproblem result is
// keyed by the revision it was computed at, so a cache entry can never outlive
// the state it describes. It is also the basis for the consistency token the
// API hands back to clients, which is how the "new enemy" problem is avoided --
// a client that has just revoked access can demand a read at or after the
// revision that revocation created.
type Revision uint64

type Store interface {
	Write(ctx context.Context, add []Tuple, remove []Tuple) (Revision, error)
	// Read returns tuples matching object+relation at the given revision.
	Read(ctx context.Context, rev Revision, object Object, relation string) ([]Tuple, error)
	Head(ctx context.Context) (Revision, error)
}

// ParseSubject accepts "user:ayush" or "group:eng#member".
func ParseSubject(s string) (Subject, error) {
	ref := s
	relation := ""
	if i := strings.Index(s, "#"); i >= 0 {
		ref, relation = s[:i], s[i+1:]
		if relation == "" {
			return Subject{}, fmt.Errorf("storage: %q has an empty userset relation", s)
		}
	}
	obj, err := ParseObject(ref)
	if err != nil {
		return Subject{}, err
	}
	return Subject{Object: obj, Relation: relation}, nil
}

func ParseObject(s string) (Object, error) {
	i := strings.Index(s, ":")
	if i <= 0 || i == len(s)-1 {
		return Object{}, fmt.Errorf("storage: %q is not type:id", s)
	}
	return Object{Type: s[:i], ID: s[i+1:]}, nil
}

// Memory is an in-process Store used by tests and benchmarks.
//
// Tuples are kept with the revision that created them and, once removed, the
// revision that killed them, so a read at an older revision still sees the
// world as it was. That is the same idea as MVCC, and it is deliberately the
// same shape the Postgres-backed store will have, so swapping the two does not
// change the engine above it.
type Memory struct {
	mu   sync.RWMutex
	head Revision
	rows []row
	// index maps object+relation to row offsets so a read does not scan.
	index map[string][]int
}

type row struct {
	tuple      Tuple
	created    Revision
	deleted    Revision // 0 means live
}

func NewMemory() *Memory {
	return &Memory{index: map[string][]int{}}
}

func key(o Object, relation string) string { return o.String() + "#" + relation }

func (m *Memory) Write(ctx context.Context, add []Tuple, remove []Tuple) (Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.head++
	rev := m.head

	for _, t := range remove {
		for _, i := range m.index[key(t.Object, t.Relation)] {
			r := &m.rows[i]
			if r.deleted == 0 && r.tuple.Subject == t.Subject {
				r.deleted = rev
			}
		}
	}
	for _, t := range add {
		// Writing a tuple that is already live is a no-op rather than a
		// duplicate, so a retried write cannot change the answer.
		dup := false
		for _, i := range m.index[key(t.Object, t.Relation)] {
			r := m.rows[i]
			if r.deleted == 0 && r.tuple.Subject == t.Subject {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		m.rows = append(m.rows, row{tuple: t, created: rev})
		k := key(t.Object, t.Relation)
		m.index[k] = append(m.index[k], len(m.rows)-1)
	}
	return rev, nil
}

func (m *Memory) Read(ctx context.Context, rev Revision, object Object, relation string) ([]Tuple, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Tuple
	for _, i := range m.index[key(object, relation)] {
		r := m.rows[i]
		// Visible iff it existed by rev and had not yet been deleted at rev.
		if r.created <= rev && (r.deleted == 0 || r.deleted > rev) {
			out = append(out, r.tuple)
		}
	}
	return out, nil
}

func (m *Memory) Head(ctx context.Context) (Revision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.head, nil
}
