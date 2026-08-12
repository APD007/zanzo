package storage

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// The same assertions run against every Store. A second implementation is only
// useful if it is actually interchangeable, and "it compiles against the
// interface" does not establish that -- the semantics that matter here
// (revision visibility, idempotent writes, tombstones rather than deletes) are
// invisible to the type system.

type storeFactory struct {
	name string
	new  func(t *testing.T) Store
}

func stores(t *testing.T) []storeFactory {
	out := []storeFactory{
		{"memory", func(t *testing.T) Store { return NewMemory() }},
	}
	dsn := os.Getenv("ZANZO_TEST_POSTGRES")
	if dsn == "" {
		t.Log("ZANZO_TEST_POSTGRES not set; skipping the Postgres conformance run")
		return out
	}
	out = append(out, storeFactory{"postgres", func(t *testing.T) Store {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		p := NewPostgres(db)
		ctx := context.Background()
		if err := p.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		// Each run starts from empty so revision numbers are predictable.
		if _, err := db.ExecContext(ctx, `TRUNCATE tuples, revisions RESTART IDENTITY`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return p
	}})
	return out
}

func tuple(object, relation, subject string) Tuple {
	o, _ := ParseObject(object)
	s, _ := ParseSubject(subject)
	return Tuple{Object: o, Relation: relation, Subject: s}
}

func mustRead(t *testing.T, s Store, rev Revision, object, relation string) []Tuple {
	t.Helper()
	o, _ := ParseObject(object)
	got, err := s.Read(context.Background(), rev, o, relation)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

func TestStoreConformance(t *testing.T) {
	for _, f := range stores(t) {
		t.Run(f.name, func(t *testing.T) {
			ctx := context.Background()

			t.Run("write then read at that revision", func(t *testing.T) {
				s := f.new(t)
				rev, err := s.Write(ctx, []Tuple{tuple("doc:a", "viewer", "user:riya")}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := mustRead(t, s, rev, "doc:a", "viewer"); len(got) != 1 {
					t.Fatalf("want 1 tuple, got %d", len(got))
				}
			})

			t.Run("a write is invisible to an earlier revision", func(t *testing.T) {
				s := f.new(t)
				first, err := s.Write(ctx, []Tuple{tuple("doc:a", "viewer", "user:riya")}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Write(ctx, []Tuple{tuple("doc:a", "viewer", "user:ayush")}, nil); err != nil {
					t.Fatal(err)
				}
				if got := mustRead(t, s, first, "doc:a", "viewer"); len(got) != 1 {
					t.Fatalf("revision %d should see 1 tuple, saw %d", first, len(got))
				}
			})

			t.Run("removal tombstones rather than deletes", func(t *testing.T) {
				s := f.new(t)
				tup := tuple("doc:a", "viewer", "user:riya")
				granted, err := s.Write(ctx, []Tuple{tup}, nil)
				if err != nil {
					t.Fatal(err)
				}
				revoked, err := s.Write(ctx, nil, []Tuple{tup})
				if err != nil {
					t.Fatal(err)
				}
				if got := mustRead(t, s, revoked, "doc:a", "viewer"); len(got) != 0 {
					t.Fatalf("after revocation want 0, got %d", len(got))
				}
				// History survives: the grant is still true at its own revision.
				if got := mustRead(t, s, granted, "doc:a", "viewer"); len(got) != 1 {
					t.Fatalf("history was destroyed: want 1 at rev %d, got %d", granted, len(got))
				}
			})

			t.Run("re-writing a live tuple is idempotent", func(t *testing.T) {
				s := f.new(t)
				tup := tuple("doc:a", "viewer", "user:riya")
				if _, err := s.Write(ctx, []Tuple{tup}, nil); err != nil {
					t.Fatal(err)
				}
				rev, err := s.Write(ctx, []Tuple{tup}, nil)
				if err != nil {
					t.Fatalf("a retried write must not error: %v", err)
				}
				if got := mustRead(t, s, rev, "doc:a", "viewer"); len(got) != 1 {
					t.Fatalf("duplicate row created: got %d", len(got))
				}
			})

			t.Run("a relationship may be re-granted after revocation", func(t *testing.T) {
				s := f.new(t)
				tup := tuple("doc:a", "viewer", "user:riya")
				if _, err := s.Write(ctx, []Tuple{tup}, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Write(ctx, nil, []Tuple{tup}); err != nil {
					t.Fatal(err)
				}
				rev, err := s.Write(ctx, []Tuple{tup}, nil)
				if err != nil {
					t.Fatalf("re-grant failed: %v", err)
				}
				if got := mustRead(t, s, rev, "doc:a", "viewer"); len(got) != 1 {
					t.Fatalf("want 1 live tuple after re-grant, got %d", len(got))
				}
			})

			t.Run("userset subjects round-trip", func(t *testing.T) {
				s := f.new(t)
				rev, err := s.Write(ctx, []Tuple{tuple("doc:a", "viewer", "group:eng#member")}, nil)
				if err != nil {
					t.Fatal(err)
				}
				got := mustRead(t, s, rev, "doc:a", "viewer")
				if len(got) != 1 {
					t.Fatalf("want 1, got %d", len(got))
				}
				if !got[0].Subject.IsUserset() || got[0].Subject.String() != "group:eng#member" {
					t.Fatalf("userset mangled: %q", got[0].Subject.String())
				}
			})

			t.Run("removal and addition land at one revision", func(t *testing.T) {
				s := f.new(t)
				old := tuple("doc:a", "viewer", "user:riya")
				if _, err := s.Write(ctx, []Tuple{old}, nil); err != nil {
					t.Fatal(err)
				}
				rev, err := s.Write(ctx,
					[]Tuple{tuple("doc:a", "viewer", "user:ayush")},
					[]Tuple{old})
				if err != nil {
					t.Fatal(err)
				}
				got := mustRead(t, s, rev, "doc:a", "viewer")
				if len(got) != 1 || got[0].Subject.String() != "user:ayush" {
					t.Fatalf("swap did not apply atomically: %+v", got)
				}
			})

			t.Run("head advances with each write", func(t *testing.T) {
				s := f.new(t)
				before, err := s.Head(ctx)
				if err != nil {
					t.Fatal(err)
				}
				rev, err := s.Write(ctx, []Tuple{tuple("doc:h", "viewer", "user:riya")}, nil)
				if err != nil {
					t.Fatal(err)
				}
				after, err := s.Head(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !(after > before) || after != rev {
					t.Fatalf("head=%d before=%d write=%d", after, before, rev)
				}
			})
		})
	}
}
