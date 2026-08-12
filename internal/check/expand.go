package check

import (
	"context"
	"fmt"

	"github.com/APD007/zanzo/internal/schema"
	"github.com/APD007/zanzo/internal/storage"
)

// Expand returns the tree of usersets that define a relation on an object,
// without reference to any particular subject.
//
// Check answers "may Ayush view this?" with a boolean, which is the right
// answer for an application and a useless one for a human trying to work out
// why. Expand answers "who can view this, and through what path?" -- the
// question asked by a permissions dialog, an audit, and every support ticket
// that begins "why can this person see my document".
//
// It deliberately does not filter by subject. Filtering is what Check does; if
// Expand also filtered, the two would eventually disagree.
type TreeNode struct {
	// Type is "union", "intersection", "exclusion", "leaf" or "tupleset".
	Type     string      `json:"type"`
	Object   string      `json:"object,omitempty"`
	Relation string      `json:"relation,omitempty"`
	Subjects []string    `json:"subjects,omitempty"`
	Children []*TreeNode `json:"children,omitempty"`
}

type ExpandRequest struct {
	Object   storage.Object
	Relation string
	Revision storage.Revision
}

type ExpandResult struct {
	Tree     *TreeNode
	Revision storage.Revision
}

func (e *Engine) Expand(ctx context.Context, req ExpandRequest) (ExpandResult, error) {
	if req.Revision == 0 {
		head, err := e.Store.Head(ctx)
		if err != nil {
			return ExpandResult{}, err
		}
		req.Revision = head
	}
	visited := map[string]bool{}
	tree, err := e.expand(ctx, req.Object, req.Relation, req.Revision, 0, visited)
	if err != nil {
		return ExpandResult{}, err
	}
	return ExpandResult{Tree: tree, Revision: req.Revision}, nil
}

func (e *Engine) expand(ctx context.Context, object storage.Object, relation string,
	rev storage.Revision, depth int, visited map[string]bool) (*TreeNode, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	maxDepth := e.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if depth > maxDepth {
		return nil, fmt.Errorf("%w at %s#%s", ErrDepthExceeded, object, relation)
	}
	// A cycle is drawn as a leaf rather than followed. Rendering it as an
	// infinite tree would be accurate and unusable.
	key := object.String() + "#" + relation
	if visited[key] {
		return &TreeNode{Type: "leaf", Object: object.String(), Relation: relation}, nil
	}
	visited[key] = true
	defer delete(visited, key)

	rel, err := e.Schema.Lookup(object.Type, relation)
	if err != nil {
		return nil, err
	}
	rw := rel.Rewrite
	if rw == nil {
		rw = schema.This{}
	}
	return e.expandRewrite(ctx, object, relation, rw, rev, depth, visited)
}

func (e *Engine) expandRewrite(ctx context.Context, object storage.Object, relation string,
	rw schema.Rewrite, rev storage.Revision, depth int, visited map[string]bool) (*TreeNode, error) {

	switch r := rw.(type) {
	case schema.This:
		tuples, err := e.Store.Read(ctx, rev, object, relation)
		if err != nil {
			return nil, err
		}
		node := &TreeNode{Type: "leaf", Object: object.String(), Relation: relation}
		for _, t := range tuples {
			node.Subjects = append(node.Subjects, t.Subject.String())
			if t.Subject.IsUserset() {
				child, err := e.expand(ctx, t.Subject.Object, t.Subject.Relation, rev, depth+1, visited)
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, child)
			}
		}
		return node, nil

	case schema.ComputedUserset:
		return e.expand(ctx, object, r.Relation, rev, depth+1, visited)

	case schema.TupleToUserset:
		tuples, err := e.Store.Read(ctx, rev, object, r.Tupleset)
		if err != nil {
			return nil, err
		}
		node := &TreeNode{Type: "tupleset", Object: object.String(), Relation: r.Tupleset}
		for _, t := range tuples {
			child, err := e.expand(ctx, t.Subject.Object, r.ComputedUserset, rev, depth+1, visited)
			if err != nil {
				return nil, err
			}
			node.Children = append(node.Children, child)
		}
		return node, nil

	case schema.Union, schema.Intersection, schema.Exclusion:
		var kids []schema.Rewrite
		kind := ""
		switch t := r.(type) {
		case schema.Union:
			kind, kids = "union", t.Children
		case schema.Intersection:
			kind, kids = "intersection", t.Children
		case schema.Exclusion:
			kind, kids = "exclusion", []schema.Rewrite{t.Base, t.Subtract}
		}
		node := &TreeNode{Type: kind, Object: object.String(), Relation: relation}
		for _, k := range kids {
			child, err := e.expandRewrite(ctx, object, relation, k, rev, depth, visited)
			if err != nil {
				return nil, err
			}
			node.Children = append(node.Children, child)
		}
		return node, nil
	}
	return nil, fmt.Errorf("expand: unhandled rewrite %T", rw)
}
