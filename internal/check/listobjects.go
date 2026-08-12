package check

import (
	"context"
	"fmt"
	"sort"

	"github.com/APD007/zanzo/internal/storage"
)

// ListObjects answers the reverse question: which objects of a type does this
// subject hold a permission on?
//
// It is genuinely harder than Check, and not because of scale. Check starts at
// a known object and asks a yes/no question, so it can short-circuit the moment
// it finds a grant. ListObjects has no starting object, cannot short-circuit,
// and the answer set is unbounded.
//
// The strategy is candidate generation followed by verification:
//
//  1. Walk the reverse index outward from the subject, collecting objects that
//     *might* qualify. This pass is deliberately over-inclusive.
//  2. Check each candidate properly.
//
// Step 2 is not laziness. Exclusion and intersection cannot be evaluated while
// walking backwards -- reaching a document through its folder tells you nothing
// about whether the subject is banned on that document, and the ban lives on
// the object you have only just discovered. Verifying with the real engine
// keeps one definition of "allowed" instead of two that can drift apart, which
// is the failure mode that makes ListObjects disagree with Check and produces
// the worst class of bug this system can have: a UI that lists a document the
// user cannot open, or worse, hides one they can.
//
// The known limit is documented rather than hidden: candidate generation
// explores at most MaxDepth levels of reverse edges, so a grant buried deeper
// than that is missed. Check has the same bound, so the two agree.
type ListRequest struct {
	Subject    storage.Subject
	Permission string
	ObjectType string
	Revision   storage.Revision
	// Limit caps the returned set. Zero means no limit, which is the right
	// default for a test and the wrong one for an API.
	Limit int
}

type ListResult struct {
	Objects []storage.Object
	// Candidates is how many objects the reverse walk proposed. The ratio of
	// Objects to Candidates is the precision of the walk, and the number worth
	// watching: if it collapses, the verification step is doing the real work
	// and the reverse index is not earning its write cost.
	Candidates int
	Revision   storage.Revision
}

func (e *Engine) ListObjects(ctx context.Context, req ListRequest) (ListResult, error) {
	if req.ObjectType == "" || req.Permission == "" {
		return ListResult{}, fmt.Errorf("listobjects: object type and permission are required")
	}
	if req.Revision == 0 {
		head, err := e.Store.Head(ctx)
		if err != nil {
			return ListResult{}, err
		}
		req.Revision = head
	}

	candidates, err := e.candidates(ctx, req)
	if err != nil {
		return ListResult{}, err
	}

	// Deterministic order. Without it the same data returns different pages to
	// a paginating caller, which looks like objects appearing and vanishing.
	ordered := make([]storage.Object, 0, len(candidates))
	for o := range candidates {
		ordered = append(ordered, o)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Type != ordered[j].Type {
			return ordered[i].Type < ordered[j].Type
		}
		return ordered[i].ID < ordered[j].ID
	})

	out := ListResult{Candidates: len(ordered), Revision: req.Revision}
	for _, o := range ordered {
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
		res, err := e.Check(ctx, Request{
			Object:   o,
			Relation: req.Permission,
			Subject:  req.Subject,
			Revision: req.Revision,
		})
		if err != nil {
			return ListResult{}, err
		}
		if res.Allowed {
			out.Objects = append(out.Objects, o)
			if req.Limit > 0 && len(out.Objects) >= req.Limit {
				break
			}
		}
	}
	return out, nil
}

// reached is one node of the reverse walk: "the subject satisfies
// object#relation".
type reached struct {
	object   storage.Object
	relation string
}

func (e *Engine) candidates(ctx context.Context, req ListRequest) (map[storage.Object]struct{}, error) {
	maxDepth := e.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	out := map[storage.Object]struct{}{}
	seen := map[reached]bool{}

	// Which relations on the target type can make the permission true. Reaching
	// any of them makes an object worth verifying.
	contributing := map[string]bool{}
	for _, r := range e.Schema.Contributors(req.ObjectType, req.Permission) {
		contributing[r] = true
	}

	// The frontier starts at the concrete subject. Every tuple naming it is a
	// first step.
	type frontierItem struct {
		subject storage.Subject
		depth   int
	}
	frontier := []frontierItem{{subject: req.Subject}}

	for len(frontier) > 0 {
		item := frontier[0]
		frontier = frontier[1:]
		if item.depth > maxDepth {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		tuples, err := e.Store.ReadBySubject(ctx, req.Revision, item.subject, "")
		if err != nil {
			return nil, err
		}

		for _, t := range tuples {
			node := reached{object: t.Object, relation: t.Relation}
			if seen[node] {
				continue
			}
			seen[node] = true

			if t.Object.Type == req.ObjectType && contributing[t.Relation] {
				out[t.Object] = struct{}{}
			}

			// Anything granted to object#relation is transitively granted to
			// whoever satisfies it -- this is how nested groups unfold in
			// reverse.
			frontier = append(frontier, frontierItem{
				subject: storage.Subject{Object: t.Object, Relation: t.Relation},
				depth:   item.depth + 1,
			})

			// Inheritance, backwards. If some type says
			// `permission p = ts->relation`, then objects pointing at this one
			// through ts inherit from it. Those tuples name this object as a
			// plain subject, not a userset.
			for _, w := range e.Schema.WalksInto(t.Relation) {
				parents, err := e.Store.ReadBySubject(ctx, req.Revision,
					storage.Subject{Object: t.Object}, w.Tupleset)
				if err != nil {
					return nil, err
				}
				for _, pt := range parents {
					if pt.Object.Type != w.ObjectType {
						continue
					}
					child := reached{object: pt.Object, relation: w.Permission}
					if seen[child] {
						continue
					}
					seen[child] = true
					if pt.Object.Type == req.ObjectType && contributing[w.Permission] {
						out[pt.Object] = struct{}{}
					}
					frontier = append(frontier, frontierItem{
						subject: storage.Subject{Object: pt.Object, Relation: w.Permission},
						depth:   item.depth + 1,
					})
				}
			}
		}
	}
	return out, nil
}
