package check

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/APD007/zanzo/internal/schema"
	"github.com/APD007/zanzo/internal/storage"
)

func listSchema(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Parse(`
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
  relation editor: user | group#member
  relation banned: user

  permission edit = editor + owner
  permission view = (edit + parent->viewer) - banned
}`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ids(objs []storage.Object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.String())
	}
	sort.Strings(out)
	return out
}

func TestListObjectsFindsInheritedAndNested(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	e := New(listSchema(t), store)

	if _, err := store.Write(ctx, []storage.Tuple{
		// riya reaches d1 through two nested groups and a folder
		tup(t, "group:backend", "member", "user:riya"),
		tup(t, "group:eng", "member", "group:backend#member"),
		tup(t, "folder:eng", "viewer", "group:eng#member"),
		tup(t, "document:d1", "parent", "folder:eng"),
		// and owns d2 outright
		tup(t, "document:d2", "owner", "user:riya"),
		// d3 is inherited but riya is banned on it
		tup(t, "document:d3", "parent", "folder:eng"),
		tup(t, "document:d3", "banned", "user:riya"),
		// d4 belongs to somebody else entirely
		tup(t, "document:d4", "owner", "user:ayush"),
	}, nil); err != nil {
		t.Fatal(err)
	}

	res, err := e.ListObjects(ctx, ListRequest{
		Subject: subj(t, "user:riya"), Permission: "view", ObjectType: "document",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res.Objects)
	want := []string{"document:d1", "document:d2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v (candidates=%d)", got, want, res.Candidates)
	}
	// d3 must have been proposed and then rejected: that is the whole reason
	// the verification pass exists.
	if res.Candidates < 3 {
		t.Fatalf("expected the banned document to be proposed as a candidate, candidates=%d", res.Candidates)
	}
}

// The property that matters: ListObjects and Check must never disagree.
//
// A UI that lists a document the user cannot open -- or hides one they can --
// is the worst bug this system can produce, and it is exactly what happens when
// the reverse walk grows a second, subtly different definition of "allowed".
// This brute-forces Check over every object and demands an exact match.
func TestListObjectsAgreesWithCheck(t *testing.T) {
	const (
		docs    = 60
		folders = 8
		groups  = 6
		users   = 12
		seeds   = 25
	)
	ctx := context.Background()

	// Two sets of empty results also "agree". Track how much real signal the
	// run produced so this test cannot quietly become vacuous if the generator
	// or the schema changes.
	var totalGranted, totalRejectedCandidates int

	for seed := 0; seed < seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(seed)))
			store := storage.NewMemory()
			e := New(listSchema(t), store)

			var tuples []storage.Tuple
			// Group chains, sometimes nested, sometimes cyclic by accident.
			for g := 0; g < groups; g++ {
				for k := 0; k < rng.Intn(3); k++ {
					tuples = append(tuples, tup(t,
						fmt.Sprintf("group:g%d", g), "member",
						fmt.Sprintf("user:u%d", rng.Intn(users))))
				}
				if rng.Intn(2) == 0 {
					tuples = append(tuples, tup(t,
						fmt.Sprintf("group:g%d", g), "member",
						fmt.Sprintf("group:g%d#member", rng.Intn(groups))))
				}
			}
			for f := 0; f < folders; f++ {
				if rng.Intn(2) == 0 {
					tuples = append(tuples, tup(t,
						fmt.Sprintf("folder:f%d", f), "viewer",
						fmt.Sprintf("group:g%d#member", rng.Intn(groups))))
				}
				if rng.Intn(2) == 0 {
					tuples = append(tuples, tup(t,
						fmt.Sprintf("folder:f%d", f), "viewer",
						fmt.Sprintf("user:u%d", rng.Intn(users))))
				}
			}
			for d := 0; d < docs; d++ {
				doc := fmt.Sprintf("document:d%d", d)
				if rng.Intn(3) > 0 {
					tuples = append(tuples, tup(t, doc, "parent",
						fmt.Sprintf("folder:f%d", rng.Intn(folders))))
				}
				if rng.Intn(3) == 0 {
					tuples = append(tuples, tup(t, doc, "owner",
						fmt.Sprintf("user:u%d", rng.Intn(users))))
				}
				if rng.Intn(4) == 0 {
					tuples = append(tuples, tup(t, doc, "editor",
						fmt.Sprintf("group:g%d#member", rng.Intn(groups))))
				}
				// Bans are what make the naive reverse walk wrong.
				if rng.Intn(5) == 0 {
					tuples = append(tuples, tup(t, doc, "banned",
						fmt.Sprintf("user:u%d", rng.Intn(users))))
				}
			}
			if _, err := store.Write(ctx, tuples, nil); err != nil {
				t.Fatal(err)
			}

			for u := 0; u < users; u++ {
				subject := subj(t, fmt.Sprintf("user:u%d", u))

				// Ground truth: ask Check about every document.
				var expected []string
				for d := 0; d < docs; d++ {
					o := obj(t, fmt.Sprintf("document:d%d", d))
					res, err := e.Check(ctx, Request{Object: o, Relation: "view", Subject: subject})
					if err != nil {
						t.Fatal(err)
					}
					if res.Allowed {
						expected = append(expected, o.String())
					}
				}
				sort.Strings(expected)

				got, err := e.ListObjects(ctx, ListRequest{
					Subject: subject, Permission: "view", ObjectType: "document",
				})
				if err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(ids(got.Objects)) != fmt.Sprint(expected) {
					t.Fatalf("user u%d disagreement\n  ListObjects: %v\n  Check:       %v",
						u, ids(got.Objects), expected)
				}
				totalGranted += len(got.Objects)
				totalRejectedCandidates += got.Candidates - len(got.Objects)
			}
		})
	}

	if totalGranted < 100 {
		t.Fatalf("only %d grants across the whole run; the generator is not "+
			"producing enough access for this comparison to mean anything", totalGranted)
	}
	if totalRejectedCandidates < 10 {
		t.Fatalf("only %d candidates were rejected by verification; the reverse "+
			"walk is not over-proposing, so the verify step is untested", totalRejectedCandidates)
	}
	t.Logf("agreement held: %d grants, %d candidates rejected by verification",
		totalGranted, totalRejectedCandidates)
}

func TestListObjectsLimit(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	e := New(listSchema(t), store)

	var tuples []storage.Tuple
	for d := 0; d < 20; d++ {
		tuples = append(tuples, tup(t, fmt.Sprintf("document:d%d", d), "owner", "user:riya"))
	}
	if _, err := store.Write(ctx, tuples, nil); err != nil {
		t.Fatal(err)
	}
	res, err := e.ListObjects(ctx, ListRequest{
		Subject: subj(t, "user:riya"), Permission: "view", ObjectType: "document", Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 5 {
		t.Fatalf("limit ignored: got %d", len(res.Objects))
	}
}

// Revocation must be reflected in the reverse direction too, at the revision
// that performed it.
func TestListObjectsRespectsRevisions(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	e := New(listSchema(t), store)

	grant := tup(t, "document:d1", "owner", "user:riya")
	before, err := store.Write(ctx, []storage.Tuple{grant}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Write(ctx, nil, []storage.Tuple{grant})
	if err != nil {
		t.Fatal(err)
	}

	old, err := e.ListObjects(ctx, ListRequest{
		Subject: subj(t, "user:riya"), Permission: "view",
		ObjectType: "document", Revision: before,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(old.Objects) != 1 {
		t.Fatalf("historic revision lost the grant: %v", ids(old.Objects))
	}

	now, err := e.ListObjects(ctx, ListRequest{
		Subject: subj(t, "user:riya"), Permission: "view",
		ObjectType: "document", Revision: after,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(now.Objects) != 0 {
		t.Fatalf("revoked grant still listed: %v", ids(now.Objects))
	}
}
