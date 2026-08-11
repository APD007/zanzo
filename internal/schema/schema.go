// Package schema models a Zanzibar-style authorization schema.
//
// Permissions are not stored. What is stored is relationships -- "user:ayush is
// an editor of doc:readme", "folder:eng is the parent of doc:readme" -- and the
// schema describes how to walk those relationships to answer a question like
// "may user:ayush view doc:readme?".
//
// That walk is a graph traversal, which is why a permission check is really a
// reachability query with a latency budget.
package schema

import "fmt"

// Rewrite is how a relation computes its members. The nil Rewrite means the
// relation is satisfied only by tuples stored against it directly.
type Rewrite interface{ isRewrite() }

// This matches subjects written directly against the relation being evaluated.
type This struct{}

// ComputedUserset delegates to another relation on the *same* object:
// "an editor is also a viewer" is Union{This, ComputedUserset{"editor"}}.
type ComputedUserset struct{ Relation string }

// TupleToUserset walks an edge and then evaluates a relation on whatever it
// finds: "you may view a document if you may view its parent folder" is
// TupleToUserset{Tupleset: "parent", ComputedUserset: "view"}.
//
// This is the operator that makes inheritance work, and the one that makes
// checks unboundedly deep, which is why the engine needs a depth limit.
type TupleToUserset struct {
	Tupleset        string
	ComputedUserset string
}

type Union struct{ Children []Rewrite }
type Intersection struct{ Children []Rewrite }

// Exclusion is Base minus Subtract. Order matters, unlike the other two.
type Exclusion struct{ Base, Subtract Rewrite }

func (This) isRewrite()            {}
func (ComputedUserset) isRewrite() {}
func (TupleToUserset) isRewrite()  {}
func (Union) isRewrite()           {}
func (Intersection) isRewrite()    {}
func (Exclusion) isRewrite()       {}

type Relation struct {
	Name string
	// Rewrite nil means direct tuples only.
	Rewrite Rewrite
}

type Definition struct {
	Name      string
	Relations map[string]*Relation
}

type Schema struct {
	Definitions map[string]*Definition
}

func New() *Schema {
	return &Schema{Definitions: map[string]*Definition{}}
}

// Lookup resolves a relation on an object type. A check against a relation the
// schema does not define must fail closed rather than be treated as empty --
// a typo in a schema should deny, never silently permit.
func (s *Schema) Lookup(objectType, relation string) (*Relation, error) {
	def, ok := s.Definitions[objectType]
	if !ok {
		return nil, fmt.Errorf("schema: unknown object type %q", objectType)
	}
	rel, ok := def.Relations[relation]
	if !ok {
		return nil, fmt.Errorf("schema: %s has no relation %q", objectType, relation)
	}
	return rel, nil
}
