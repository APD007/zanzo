package check

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/APD007/zanzo/internal/schema"
	"github.com/APD007/zanzo/internal/storage"
)

// A schema close to what a document product actually needs:
//
//	document:
//	  parent  = direct
//	  owner   = direct
//	  editor  = direct + owner
//	  viewer  = (direct + editor + parent->viewer) - banned
//	  banned  = direct
//	folder:
//	  viewer  = direct
//	group:
//	  member  = direct   (subjects may themselves be group:x#member)
func testSchema() *schema.Schema {
	s := schema.New()
	s.Definitions["group"] = &schema.Definition{
		Name:      "group",
		Relations: map[string]*schema.Relation{"member": {Name: "member"}},
	}
	s.Definitions["folder"] = &schema.Definition{
		Name:      "folder",
		Relations: map[string]*schema.Relation{"viewer": {Name: "viewer"}},
	}
	s.Definitions["document"] = &schema.Definition{
		Name: "document",
		Relations: map[string]*schema.Relation{
			"parent": {Name: "parent"},
			"owner":  {Name: "owner"},
			"banned": {Name: "banned"},
			"editor": {Name: "editor", Rewrite: schema.Union{Children: []schema.Rewrite{
				schema.This{}, schema.ComputedUserset{Relation: "owner"},
			}}},
			"viewer": {Name: "viewer", Rewrite: schema.Exclusion{
				Base: schema.Union{Children: []schema.Rewrite{
					schema.This{},
					schema.ComputedUserset{Relation: "editor"},
					schema.TupleToUserset{Tupleset: "parent", ComputedUserset: "viewer"},
				}},
				Subtract: schema.ComputedUserset{Relation: "banned"},
			}},
		},
	}
	return s
}

func mustTuple(t *testing.T, object, relation, subject string) storage.Tuple {
	t.Helper()
	o, err := storage.ParseObject(object)
	if err != nil {
		t.Fatalf("object %q: %v", object, err)
	}
	s, err := storage.ParseSubject(subject)
	if err != nil {
		t.Fatalf("subject %q: %v", subject, err)
	}
	return storage.Tuple{Object: o, Relation: relation, Subject: s}
}

func allow(t *testing.T, e *Engine, object, relation, subject string) Result {
	t.Helper()
	o, err := storage.ParseObject(object)
	if err != nil {
		t.Fatal(err)
	}
	s, err := storage.ParseSubject(subject)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Check(context.Background(), Request{Object: o, Relation: relation, Subject: s})
	if err != nil {
		t.Fatalf("check %s#%s@%s: %v", object, relation, subject, err)
	}
	return res
}

func TestCheck(t *testing.T) {
	store := storage.NewMemory()
	e := New(testSchema(), store)

	_, err := store.Write(context.Background(), []storage.Tuple{
		mustTuple(t, "document:readme", "owner", "user:ayush"),
		mustTuple(t, "document:readme", "parent", "folder:eng"),
		mustTuple(t, "folder:eng", "viewer", "group:eng#member"),
		mustTuple(t, "group:eng", "member", "group:backend#member"),
		mustTuple(t, "group:backend", "member", "user:riya"),
		mustTuple(t, "document:readme", "banned", "user:mallory"),
		mustTuple(t, "folder:eng", "viewer", "user:mallory"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name                      string
		object, relation, subject string
		want                      bool
	}{
		{"owner is an editor via computed userset", "document:readme", "editor", "user:ayush", true},
		{"owner is a viewer transitively through editor", "document:readme", "viewer", "user:ayush", true},
		{"nested group member inherits through the parent folder", "document:readme", "viewer", "user:riya", true},
		{"unrelated user is denied", "document:readme", "viewer", "user:stranger", false},
		{"exclusion beats an inherited grant", "document:readme", "viewer", "user:mallory", false},
		{"a viewer is not thereby an editor", "document:readme", "editor", "user:riya", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allow(t, e, tc.object, tc.relation, tc.subject).Allowed; got != tc.want {
				t.Fatalf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// A schema may contain a membership cycle. The engine must answer rather than
// recurse forever, and must still find a grant that exists outside the cycle.
func TestCyclicGroupsTerminate(t *testing.T) {
	store := storage.NewMemory()
	e := New(testSchema(), store)
	if _, err := store.Write(context.Background(), []storage.Tuple{
		mustTuple(t, "group:a", "member", "group:b#member"),
		mustTuple(t, "group:b", "member", "group:a#member"),
		mustTuple(t, "group:b", "member", "user:riya"),
	}, nil); err != nil {
		t.Fatal(err)
	}

	if got := allow(t, e, "group:a", "member", "user:stranger").Allowed; got {
		t.Fatal("cycle granted access to a stranger")
	}
	if got := allow(t, e, "group:a", "member", "user:riya").Allowed; !got {
		t.Fatal("cycle hid a real grant")
	}
}

// Depth is bounded even when the data, not the schema, is pathological.
func TestDepthLimitDenies(t *testing.T) {
	store := storage.NewMemory()
	e := New(testSchema(), store)
	e.MaxDepth = 8

	var tuples []storage.Tuple
	for i := 0; i < 40; i++ {
		tuples = append(tuples, mustTuple(t,
			"group:g"+itoa(i), "member", "group:g"+itoa(i+1)+"#member"))
	}
	tuples = append(tuples, mustTuple(t, "group:g40", "member", "user:riya"))
	if _, err := store.Write(context.Background(), tuples, nil); err != nil {
		t.Fatal(err)
	}

	o, _ := storage.ParseObject("group:g0")
	s, _ := storage.ParseSubject("user:riya")
	_, err := e.Check(context.Background(), Request{Object: o, Relation: "member", Subject: s})
	if !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("want ErrDepthExceeded, got %v", err)
	}
}

// An unknown relation must deny loudly rather than resolve to "no tuples".
func TestUnknownRelationFailsClosed(t *testing.T) {
	store := storage.NewMemory()
	e := New(testSchema(), store)
	o, _ := storage.ParseObject("document:readme")
	s, _ := storage.ParseSubject("user:ayush")
	res, err := e.Check(context.Background(), Request{Object: o, Relation: "typo", Subject: s})
	if err == nil {
		t.Fatal("want an error for an undefined relation")
	}
	if res.Allowed {
		t.Fatal("denied checks must never report Allowed")
	}
	if !strings.Contains(err.Error(), "no relation") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// A revision pins the answer. A grant written after the pinned revision must
// be invisible to a check at that revision -- this is what makes it safe to
// cache a subproblem result keyed by revision.
func TestRevisionPinsTheAnswer(t *testing.T) {
	store := storage.NewMemory()
	e := New(testSchema(), store)
	ctx := context.Background()

	before, err := store.Write(ctx, []storage.Tuple{
		mustTuple(t, "document:readme", "owner", "user:ayush"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(ctx, []storage.Tuple{
		mustTuple(t, "document:readme", "owner", "user:riya"),
	}, nil); err != nil {
		t.Fatal(err)
	}

	o, _ := storage.ParseObject("document:readme")
	s, _ := storage.ParseSubject("user:riya")

	old, err := e.Check(ctx, Request{Object: o, Relation: "viewer", Subject: s, Revision: before})
	if err != nil {
		t.Fatal(err)
	}
	if old.Allowed {
		t.Fatal("a check at an older revision saw a later write")
	}
	now, err := e.Check(ctx, Request{Object: o, Relation: "viewer", Subject: s})
	if err != nil {
		t.Fatal(err)
	}
	if !now.Allowed {
		t.Fatal("a check at head missed a committed write")
	}
}

// Revoking must actually revoke at the revision the revocation created.
func TestRevocationTakesEffect(t *testing.T) {
	store := storage.NewMemory()
	e := New(testSchema(), store)
	ctx := context.Background()
	grant := mustTuple(t, "document:readme", "owner", "user:riya")
	if _, err := store.Write(ctx, []storage.Tuple{grant}, nil); err != nil {
		t.Fatal(err)
	}
	if got := allow(t, e, "document:readme", "viewer", "user:riya").Allowed; !got {
		t.Fatal("grant did not take")
	}
	if _, err := store.Write(ctx, nil, []storage.Tuple{grant}); err != nil {
		t.Fatal(err)
	}
	if got := allow(t, e, "document:readme", "viewer", "user:riya").Allowed; got {
		t.Fatal("revoked subject still allowed")
	}
}

// The diamond: two groups both grant, and both resolve through the same
// subtree. Memoisation should collapse the shared work.
func TestMemoisationCollapsesSharedSubtrees(t *testing.T) {
	store := storage.NewMemory()
	e := New(testSchema(), store)
	if _, err := store.Write(context.Background(), []storage.Tuple{
		mustTuple(t, "document:readme", "parent", "folder:eng"),
		mustTuple(t, "folder:eng", "viewer", "group:a#member"),
		mustTuple(t, "folder:eng", "viewer", "group:b#member"),
		mustTuple(t, "group:a", "member", "group:shared#member"),
		mustTuple(t, "group:b", "member", "group:shared#member"),
		mustTuple(t, "group:shared", "member", "user:nobody"),
	}, nil); err != nil {
		t.Fatal(err)
	}
	// user:stranger forces the full search: no early exit anywhere.
	res := allow(t, e, "document:readme", "viewer", "user:stranger")
	if res.Allowed {
		t.Fatal("stranger allowed")
	}
	if res.MemoHits == 0 {
		t.Fatalf("expected the shared subtree to be memoised, got %+v", res)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
