package check

import (
	"context"
	"testing"

	"github.com/APD007/zanzo/internal/schema"
	"github.com/APD007/zanzo/internal/storage"
)

// The new-enemy problem, from the Zanzibar paper.
//
// The name comes from the scenario that motivates it: you remove someone from
// a folder, then move a sensitive document into that folder. Each write is
// fine in isolation. The bug is a *read* that runs against a snapshot older
// than the removal -- the "new enemy" sees the new document because the system
// answered using an ACL that no longer exists.
//
// It is the reason Zanzibar hands out consistency tokens rather than simply
// caching by (object, relation, subject). These tests pin both halves: that
// the obvious cache is genuinely unsafe, and that the revision-keyed one is
// not. Asserting only the second would prove nothing, because a cache that
// never hits also passes.

func newEnemySchema(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Parse(`
definition user {}

definition folder {
  relation viewer: user
}

definition document {
  relation parent: folder
  relation viewer: user
  permission view = viewer + parent->viewer
}`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func obj(t *testing.T, s string) storage.Object {
	t.Helper()
	o, err := storage.ParseObject(s)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func subj(t *testing.T, s string) storage.Subject {
	t.Helper()
	x, err := storage.ParseSubject(s)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func tup(t *testing.T, object, relation, subject string) storage.Tuple {
	t.Helper()
	return storage.Tuple{Object: obj(t, object), Relation: relation, Subject: subj(t, subject)}
}

// run the exact scenario against whichever cache is supplied.
func runNewEnemy(t *testing.T, cache Cache) (allowedAfterRevoke bool, revokeRev storage.Revision) {
	t.Helper()
	ctx := context.Background()
	store := storage.NewMemory()
	e := New(newEnemySchema(t), store)
	e.Cache = cache

	// Mallory can see the folder, and a harmless document sits inside it.
	grant := tup(t, "folder:eng", "viewer", "user:mallory")
	if _, err := store.Write(ctx, []storage.Tuple{
		grant,
		tup(t, "document:harmless", "parent", "folder:eng"),
	}, nil); err != nil {
		t.Fatal(err)
	}

	// A legitimate read warms the cache for folder:eng#viewer@user:mallory.
	res, err := e.Check(ctx, Request{
		Object: obj(t, "document:harmless"), Relation: "view", Subject: subj(t, "user:mallory"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("setup is wrong: mallory should see the harmless document")
	}

	// Mallory is removed from the folder...
	revokeRev, err = store.Write(ctx, nil, []storage.Tuple{grant})
	if err != nil {
		t.Fatal(err)
	}
	// ...and only then does the secret get moved in.
	if _, err := store.Write(ctx, []storage.Tuple{
		tup(t, "document:secret", "parent", "folder:eng"),
	}, nil); err != nil {
		t.Fatal(err)
	}

	after, err := e.Check(ctx, Request{
		Object: obj(t, "document:secret"), Relation: "view", Subject: subj(t, "user:mallory"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return after.Allowed, revokeRev
}

// The naive cache must fail. If this test ever passes, the demonstration below
// it has stopped demonstrating anything and the revision-keyed result is no
// longer evidence of correctness.
func TestNaiveCacheExhibitsNewEnemy(t *testing.T) {
	allowed, _ := runNewEnemy(t, NewNaiveCache())
	if !allowed {
		t.Fatal("expected the naive cache to leak the revoked grant; " +
			"if it no longer does, this scenario has stopped exercising the bug")
	}
	t.Log("naive cache: mallory can read document:secret after being revoked (the bug)")
}

func TestRevisionKeyedCachePreventsNewEnemy(t *testing.T) {
	allowed, _ := runNewEnemy(t, NewRevisionKeyed())
	if allowed {
		t.Fatal("revision-keyed cache served a revoked grant")
	}
}

// And with no cache at all, to show the scenario itself is sound rather than
// an artefact of caching.
func TestUncachedIsCorrect(t *testing.T) {
	allowed, _ := runNewEnemy(t, nil)
	if allowed {
		t.Fatal("uncached engine allowed a revoked subject")
	}
}

// A caller who has just performed a revocation can force the engine to answer
// no earlier than that write, even if some replica were lagging.
func TestAtLeastAsFreshHonoursTheToken(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	e := New(newEnemySchema(t), store)
	e.Cache = NewRevisionKeyed()

	grant := tup(t, "document:readme", "viewer", "user:mallory")
	early, err := store.Write(ctx, []storage.Tuple{grant}, nil)
	if err != nil {
		t.Fatal(err)
	}
	revoke, err := store.Write(ctx, nil, []storage.Tuple{grant})
	if err != nil {
		t.Fatal(err)
	}

	// Reading at the pre-revocation snapshot still allows: that snapshot is a
	// real point in history, and the engine is not lying about it.
	stale, err := e.Check(ctx, Request{
		Object: obj(t, "document:readme"), Relation: "view",
		Subject: subj(t, "user:mallory"), Revision: early,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Allowed {
		t.Fatal("a read pinned before the revocation should still see the grant")
	}

	// Presenting the revocation's token forbids answering from anything older.
	fresh, err := e.Check(ctx, Request{
		Object: obj(t, "document:readme"), Relation: "view",
		Subject:     subj(t, "user:mallory"),
		Consistency: AtLeastAsFresh, Token: Token{Revision: revoke},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Allowed {
		t.Fatal("AtLeastAsFresh answered from a snapshot older than the token")
	}
	if fresh.Revision < revoke {
		t.Fatalf("answered at revision %d, older than the token %d", fresh.Revision, revoke)
	}
}

// The cache must actually be used, otherwise the safety tests above prove
// nothing: a cache that never hits is trivially correct and useless.
func TestRevisionKeyedCacheActuallyHits(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	e := New(newEnemySchema(t), store)
	cache := NewRevisionKeyed()
	e.Cache = cache

	if _, err := store.Write(ctx, []storage.Tuple{
		tup(t, "folder:eng", "viewer", "user:riya"),
		tup(t, "document:a", "parent", "folder:eng"),
		tup(t, "document:b", "parent", "folder:eng"),
	}, nil); err != nil {
		t.Fatal(err)
	}

	req := Request{Object: obj(t, "document:a"), Relation: "view", Subject: subj(t, "user:riya")}
	if _, err := e.Check(ctx, req); err != nil {
		t.Fatal(err)
	}
	// document:b resolves through the same folder subproblem, which is now
	// cached at this revision.
	second, err := e.Check(ctx, Request{
		Object: obj(t, "document:b"), Relation: "view", Subject: subj(t, "user:riya"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheHits == 0 {
		t.Fatalf("expected a cross-request cache hit on the shared folder, got %+v", second)
	}
	if !second.Allowed {
		t.Fatal("cached path returned the wrong answer")
	}
}
