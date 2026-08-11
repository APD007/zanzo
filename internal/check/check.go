// Package check answers "may this subject perform this relation on this
// object?" by expanding the schema's rewrite rules over stored tuples.
//
// Three properties matter more than speed here:
//
//   - It fails closed. Any error -- unknown type, depth exceeded, storage
//     failure -- denies. An authorization engine that answers "allow" when it
//     is confused is worse than one that is down.
//   - It terminates. Schemas permit cycles (group A contains group B contains
//     group A) and TupleToUserset makes depth unbounded, so the engine carries
//     both a visited set and a depth limit.
//   - It is memoised per request. A diamond in the graph (two groups both
//     granting the same permission) otherwise re-expands the shared subtree.
package check

import (
	"context"
	"errors"
	"fmt"

	"github.com/APD007/zanzo/internal/schema"
	"github.com/APD007/zanzo/internal/storage"
)

// DefaultMaxDepth bounds recursion. Real schemas nest far shallower than this;
// hitting it means a cycle the visited set could not catch or a pathological
// schema, and either way the safe answer is to deny.
const DefaultMaxDepth = 32

var ErrDepthExceeded = errors.New("check: max depth exceeded")

type Engine struct {
	Schema   *schema.Schema
	Store    storage.Store
	MaxDepth int
	// Cache is optional and shared across requests. Safe only because every
	// key carries the revision it was computed at; see cache.go.
	Cache Cache
}

// Consistency selects the snapshot a check runs against. The trade is latency
// against staleness, and only the caller knows which one they need: a UI
// listing tolerates a stale read, the request right after a revocation does not.
type Consistency int

const (
	// MinimizeLatency reads at whatever revision is already current. Cheapest,
	// and may serve a cached answer from a slightly older snapshot.
	MinimizeLatency Consistency = iota
	// AtLeastAsFresh pins the check to at least the revision carried by the
	// caller's token. This is what closes the new-enemy hole: a client that
	// has just revoked access presents the token from that write, and the
	// engine may not answer from anything older.
	AtLeastAsFresh
	// FullConsistency always reads at head. Correct, and the most expensive.
	FullConsistency
)

// Token is an opaque consistency token -- Zanzibar calls it a zookie. Clients
// treat it as a blob; it is handed out by writes and handed back on reads.
type Token struct{ Revision storage.Revision }

func New(s *schema.Schema, st storage.Store) *Engine {
	return &Engine{Schema: s, Store: st, MaxDepth: DefaultMaxDepth}
}

type Request struct {
	Object   storage.Object
	Relation string
	Subject  storage.Subject
	// Revision pins the whole check to one snapshot. Every tuple read below
	// uses it, so a check cannot observe a write that lands mid-evaluation and
	// return an answer that was never true at any single point in time.
	Revision storage.Revision

	Consistency Consistency
	// Token is the caller's zookie, honoured when Consistency is AtLeastAsFresh.
	Token Token
}

// Result carries the decision plus enough detail to explain and profile it.
type Result struct {
	Allowed bool
	// Expansions counts subproblems actually evaluated (memo hits excluded).
	Expansions int
	MemoHits   int
	CacheHits  int
	MaxDepth   int
	// Revision is the snapshot the answer was computed at. Callers echo it
	// back as a token on the next request that must not regress.
	Revision storage.Revision
}

type memoKey struct {
	object   storage.Object
	relation string
	subject  string
}

type evaluation struct {
	engine *Engine
	req    Request
	memo   map[memoKey]bool
	// visiting guards against cycles: a subproblem already on the stack
	// contributes nothing new, so it resolves to false rather than recursing.
	visiting map[memoKey]bool
	result   Result
}

func (e *Engine) Check(ctx context.Context, req Request) (Result, error) {
	if req.Revision == 0 {
		head, err := e.Store.Head(ctx)
		if err != nil {
			return Result{}, err
		}
		switch req.Consistency {
		case AtLeastAsFresh:
			// Never answer from a snapshot older than what the caller has
			// already observed. head is normally ahead of the token; if a
			// replica lagged behind it, reading at the token is what keeps a
			// just-revoked grant from reappearing.
			req.Revision = head
			if req.Token.Revision > head {
				req.Revision = req.Token.Revision
			}
		default:
			req.Revision = head
		}
	}
	ev := &evaluation{
		engine:   e,
		req:      req,
		memo:     map[memoKey]bool{},
		visiting: map[memoKey]bool{},
	}
	allowed, err := ev.check(ctx, req.Object, req.Relation, 0)
	if err != nil {
		return Result{}, err
	}
	ev.result.Allowed = allowed
	ev.result.Revision = req.Revision
	return ev.result, nil
}

func (ev *evaluation) check(ctx context.Context, object storage.Object, relation string, depth int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	maxDepth := ev.engine.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if depth > maxDepth {
		return false, fmt.Errorf("%w at %s#%s", ErrDepthExceeded, object, relation)
	}
	if depth > ev.result.MaxDepth {
		ev.result.MaxDepth = depth
	}

	k := memoKey{object: object, relation: relation, subject: ev.req.Subject.String()}
	if got, ok := ev.memo[k]; ok {
		ev.result.MemoHits++
		return got, nil
	}
	ck := Key{
		Object:   object,
		Relation: relation,
		Subject:  ev.req.Subject.String(),
		Revision: ev.req.Revision,
	}
	if ev.engine.Cache != nil {
		if got, ok := ev.engine.Cache.Get(ck); ok {
			ev.result.CacheHits++
			ev.memo[k] = got
			return got, nil
		}
	}
	if ev.visiting[k] {
		// Already being computed further up the stack. Returning false is
		// sound: a cycle can only grant access through some other branch, and
		// that branch is evaluated on its own.
		return false, nil
	}
	ev.visiting[k] = true
	defer delete(ev.visiting, k)
	ev.result.Expansions++

	rel, err := ev.engine.Schema.Lookup(object.Type, relation)
	if err != nil {
		return false, err
	}
	rw := rel.Rewrite
	if rw == nil {
		rw = schema.This{}
	}
	allowed, err := ev.eval(ctx, object, relation, rw, depth)
	if err != nil {
		return false, err
	}
	ev.memo[k] = allowed
	if ev.engine.Cache != nil {
		ev.engine.Cache.Set(ck, allowed)
	}
	return allowed, nil
}

func (ev *evaluation) eval(ctx context.Context, object storage.Object, relation string, rw schema.Rewrite, depth int) (bool, error) {
	switch r := rw.(type) {

	case schema.This:
		tuples, err := ev.engine.Store.Read(ctx, ev.req.Revision, object, relation)
		if err != nil {
			return false, err
		}
		for _, t := range tuples {
			if !t.Subject.IsUserset() {
				if t.Subject == ev.req.Subject {
					return true, nil
				}
				continue
			}
			// group:eng#member -- expand the userset. This is where nested
			// groups resolve, and where a cyclic schema would loop without
			// the visited set.
			ok, err := ev.check(ctx, t.Subject.Object, t.Subject.Relation, depth+1)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil

	case schema.ComputedUserset:
		return ev.check(ctx, object, r.Relation, depth+1)

	case schema.TupleToUserset:
		// Walk every edge stored under the tupleset relation, then ask the
		// computed relation of whatever sits on the far end.
		tuples, err := ev.engine.Store.Read(ctx, ev.req.Revision, object, r.Tupleset)
		if err != nil {
			return false, err
		}
		for _, t := range tuples {
			ok, err := ev.check(ctx, t.Subject.Object, r.ComputedUserset, depth+1)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil

	case schema.Union:
		for _, child := range r.Children {
			ok, err := ev.eval(ctx, object, relation, child, depth)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil // short-circuit on first grant
			}
		}
		return false, nil

	case schema.Intersection:
		if len(r.Children) == 0 {
			return false, nil
		}
		for _, child := range r.Children {
			ok, err := ev.eval(ctx, object, relation, child, depth)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil // short-circuit on first denial
			}
		}
		return true, nil

	case schema.Exclusion:
		base, err := ev.eval(ctx, object, relation, r.Base, depth)
		if err != nil {
			return false, err
		}
		if !base {
			return false, nil
		}
		sub, err := ev.eval(ctx, object, relation, r.Subtract, depth)
		if err != nil {
			return false, err
		}
		return !sub, nil
	}
	return false, fmt.Errorf("check: unhandled rewrite %T", rw)
}
