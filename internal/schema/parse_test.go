package schema

import (
	"strings"
	"testing"
)

const docSchema = `
definition user {}

definition group {
  relation member: user | group#member
}

definition folder {
  relation viewer: user | group#member
}

definition document {
  relation parent: folder
  relation owner: user
  relation banned: user
  relation editor: user | group#member

  // An owner edits without anyone writing an editor tuple for them.
  permission edit = editor + owner
  // Inherited from the folder, but a ban wins over any grant.
  permission view = (edit + parent->viewer) - banned
}
`

func TestParseBuildsTheExpectedShape(t *testing.T) {
	s, err := Parse(docSchema)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Definitions) != 4 {
		t.Fatalf("want 4 definitions, got %d", len(s.Definitions))
	}

	// A relation stores tuples, so it carries no rewrite.
	owner, err := s.Lookup("document", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if owner.Rewrite != nil {
		t.Fatalf("relation owner should have no rewrite, got %T", owner.Rewrite)
	}

	edit, err := s.Lookup("document", "edit")
	if err != nil {
		t.Fatal(err)
	}
	u, ok := edit.Rewrite.(Union)
	if !ok {
		t.Fatalf("edit should be a Union, got %T", edit.Rewrite)
	}
	if len(u.Children) != 2 {
		t.Fatalf("edit should union 2 children, got %d", len(u.Children))
	}

	// `(a + b) - c` must bind as an exclusion whose base is the union, not
	// a union whose last member is an exclusion.
	view, err := s.Lookup("document", "view")
	if err != nil {
		t.Fatal(err)
	}
	ex, ok := view.Rewrite.(Exclusion)
	if !ok {
		t.Fatalf("view should be an Exclusion, got %T", view.Rewrite)
	}
	if _, ok := ex.Base.(Union); !ok {
		t.Fatalf("view base should be a Union, got %T", ex.Base)
	}
	if sub, ok := ex.Subtract.(ComputedUserset); !ok || sub.Relation != "banned" {
		t.Fatalf("view should subtract banned, got %#v", ex.Subtract)
	}
}

func TestParseArrowIsTupleToUserset(t *testing.T) {
	s, err := Parse(`
definition folder { relation viewer: user }
definition document {
  relation parent: folder
  permission view = parent->viewer
}`)
	if err != nil {
		t.Fatal(err)
	}
	view, _ := s.Lookup("document", "view")
	ttu, ok := view.Rewrite.(TupleToUserset)
	if !ok {
		t.Fatalf("want TupleToUserset, got %T", view.Rewrite)
	}
	if ttu.Tupleset != "parent" || ttu.ComputedUserset != "viewer" {
		t.Fatalf("wrong walk: %#v", ttu)
	}
}

// `+` and `-` share precedence and associate left, so the exclusion must apply
// to everything accumulated to its left. Getting this wrong is a security bug,
// not a formatting one: `a + (b - c)` would let branch `a` re-grant what the
// ban was meant to remove.
func TestUnionExclusionAssociateLeft(t *testing.T) {
	s, err := Parse(`
definition document {
  relation a: user
  relation b: user
  relation c: user
  permission p = a + b - c
}`)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Lookup("document", "p")
	ex, ok := p.Rewrite.(Exclusion)
	if !ok {
		t.Fatalf("want the top node to be Exclusion, got %T", p.Rewrite)
	}
	base, ok := ex.Base.(Union)
	if !ok || len(base.Children) != 2 {
		t.Fatalf("want base Union of 2, got %#v", ex.Base)
	}
}

func TestIntersectionBindsTighterThanUnion(t *testing.T) {
	s, err := Parse(`
definition document {
  relation a: user
  relation b: user
  relation c: user
  permission p = a + b & c
}`)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Lookup("document", "p")
	u, ok := p.Rewrite.(Union)
	if !ok {
		t.Fatalf("want Union at the top, got %T", p.Rewrite)
	}
	if _, ok := u.Children[1].(Intersection); !ok {
		t.Fatalf("want `b & c` grouped, got %T", u.Children[1])
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"undefined relation", `definition d { permission p = ghost }`, "undefined relation"},
		{"undefined walk", `definition d { permission p = ghost->x }`, "walks undefined relation"},
		{"duplicate definition", "definition d {}\ndefinition d {}", "declared twice"},
		{"duplicate member", `definition d { relation a: user relation a: user }`, "twice"},
		{"unclosed definition", `definition d { relation a: user`, "never closed"},
		{"missing equals", `definition d { permission p a }`, "expected \"=\""},
		{"unbalanced paren", `definition d { relation a: user permission p = (a }`, "expected \")\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestCommentsAndBlankLinesIgnored(t *testing.T) {
	s, err := Parse(`
// leading comment
definition document {
  relation owner: user   // trailing comment

  permission edit = owner
}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup("document", "edit"); err != nil {
		t.Fatal(err)
	}
}
