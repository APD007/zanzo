package schema

// Reverse analysis of a schema.
//
// Check walks forwards: given an object, which subjects reach it. ListObjects
// walks backwards: given a subject, which objects does it reach. The schema is
// the same, but the questions need different indexes over it, which is why
// these helpers exist separately from Lookup.
//
// Everything here is conservative in one direction only: it may report that a
// relation *could* contribute to a permission when a particular object's data
// means it does not. That is safe because ListObjects verifies every candidate
// with a real Check before returning it. Being conservative the other way --
// missing a path -- would silently omit objects the caller should see, which is
// the kind of bug nobody notices until an audit.

// Contributors returns the relations on objectType whose satisfaction can make
// permission true, following ComputedUserset and Union links.
//
// Intersection children are all included: any of them could be the branch that
// matters, and the verifying Check decides. Exclusion contributes only its
// base, since the subtracted side can never grant.
func (s *Schema) Contributors(objectType, permission string) []string {
	def, ok := s.Definitions[objectType]
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walk func(relName string)
	walk = func(relName string) {
		if seen[relName] {
			return
		}
		seen[relName] = true
		out = append(out, relName)
		rel, ok := def.Relations[relName]
		if !ok || rel.Rewrite == nil {
			return
		}
		var expand func(rw Rewrite)
		expand = func(rw Rewrite) {
			switch r := rw.(type) {
			case This:
				// Tuples stored directly against relName; already recorded.
			case ComputedUserset:
				walk(r.Relation)
			case TupleToUserset:
				// Handled by Walks: it crosses to another object, so it is not
				// a contributor on this one.
			case Union:
				for _, c := range r.Children {
					expand(c)
				}
			case Intersection:
				for _, c := range r.Children {
					expand(c)
				}
			case Exclusion:
				expand(r.Base)
			}
		}
		expand(rel.Rewrite)
	}
	walk(permission)
	return out
}

// Walk is one TupleToUserset edge, reported from the perspective of the object
// type that declares it: "an object of ObjectType satisfies Permission if the
// object on the far side of Tupleset satisfies ComputedUserset".
type Walk struct {
	ObjectType      string
	Permission      string
	Tupleset        string
	ComputedUserset string
}

// WalksInto returns every TupleToUserset edge in the schema whose far-side
// relation is computedRelation.
//
// This is the reverse of inheritance. Forwards, a document asks its folder.
// Backwards, reaching a folder's viewer must find the documents that point at
// it -- and only the schema knows which relation to follow to get there.
func (s *Schema) WalksInto(computedRelation string) []Walk {
	var out []Walk
	for typeName, def := range s.Definitions {
		for relName, rel := range def.Relations {
			if rel.Rewrite == nil {
				continue
			}
			var expand func(rw Rewrite)
			expand = func(rw Rewrite) {
				switch r := rw.(type) {
				case TupleToUserset:
					if r.ComputedUserset == computedRelation {
						out = append(out, Walk{
							ObjectType:      typeName,
							Permission:      relName,
							Tupleset:        r.Tupleset,
							ComputedUserset: r.ComputedUserset,
						})
					}
				case Union:
					for _, c := range r.Children {
						expand(c)
					}
				case Intersection:
					for _, c := range r.Children {
						expand(c)
					}
				case Exclusion:
					expand(r.Base)
				}
			}
			expand(rel.Rewrite)
		}
	}
	return out
}
