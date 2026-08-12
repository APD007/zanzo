package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// Postgres implements Store on top of a relation-tuple table.
//
// Revisions are explicit columns rather than Postgres transaction ids. Reading
// "as of" an xid via a repeatable-read snapshot would work for a single
// process, but the revision here is also handed to clients as a consistency
// token and used as part of a cache key, so it has to be a stable, ordered
// value that outlives any connection or replica. A monotonic counter in its
// own table gives that.
//
// The layout mirrors the in-memory store deliberately: a row records the
// revision that created it and the revision that removed it, so a read at an
// older revision still sees the world as it was, and history is never
// destroyed. Writes are append-and-tombstone, never UPDATE-in-place on the
// tuple itself.
type Postgres struct {
	db *sql.DB
}

func NewPostgres(db *sql.DB) *Postgres { return &Postgres{db: db} }

// Schema is the DDL. Two indexes matter and they serve opposite directions:
//
//	idx_tuple_forward  (object_type, object_id, relation)
//	    the Check path -- "who is related to this object?"
//	idx_tuple_reverse  (subject_type, subject_id, subject_relation)
//	    the ListObjects path -- "what is this subject related to?"
//
// The reverse index is what makes ListObjects tractable at all, and it is pure
// write amplification for Check-only workloads, which is the trade Zanzibar
// makes with its Leopard index.
const Schema = `
CREATE TABLE IF NOT EXISTS revisions (
    id          BIGSERIAL PRIMARY KEY,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tuples (
    id                BIGSERIAL PRIMARY KEY,
    object_type       TEXT   NOT NULL,
    object_id         TEXT   NOT NULL,
    relation          TEXT   NOT NULL,
    subject_type      TEXT   NOT NULL,
    subject_id        TEXT   NOT NULL,
    -- empty string, not NULL: it is part of the identity of the row, and
    -- NULL would silently defeat the uniqueness constraint below.
    subject_relation  TEXT   NOT NULL DEFAULT '',
    created_rev       BIGINT NOT NULL,
    deleted_rev       BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_tuple_forward
    ON tuples (object_type, object_id, relation, deleted_rev, created_rev);

CREATE INDEX IF NOT EXISTS idx_tuple_reverse
    ON tuples (subject_type, subject_id, subject_relation, deleted_rev);

-- At most one live row per relationship. The partial predicate is what makes
-- this expressible: historical rows are exempt, so the same relationship may
-- be granted, revoked and granted again, but never held twice at once.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tuple_live_unique
    ON tuples (object_type, object_id, relation, subject_type, subject_id, subject_relation)
    WHERE deleted_rev = 0;
`

func (p *Postgres) Migrate(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx, Schema)
	return err
}

func (p *Postgres) Head(ctx context.Context) (Revision, error) {
	var rev sql.NullInt64
	err := p.db.QueryRowContext(ctx, `SELECT max(id) FROM revisions`).Scan(&rev)
	if err != nil {
		return 0, err
	}
	if !rev.Valid {
		return 0, nil
	}
	return Revision(rev.Int64), nil
}

// Write applies removals and additions in one transaction at one new revision.
//
// Both halves must land together: a caller revoking one grant while adding
// another is expressing a single intent, and a reader must never observe the
// half-applied state where the old grant is gone and the new one has not yet
// appeared -- or worse, the reverse.
func (p *Postgres) Write(ctx context.Context, add []Tuple, remove []Tuple) (Revision, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var rev Revision
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO revisions DEFAULT VALUES RETURNING id`).Scan(&rev); err != nil {
		return 0, fmt.Errorf("allocate revision: %w", err)
	}

	for _, t := range remove {
		if _, err := tx.ExecContext(ctx, `
            UPDATE tuples SET deleted_rev = $1
             WHERE deleted_rev = 0
               AND object_type = $2 AND object_id = $3 AND relation = $4
               AND subject_type = $5 AND subject_id = $6 AND subject_relation = $7`,
			rev, t.Object.Type, t.Object.ID, t.Relation,
			t.Subject.Object.Type, t.Subject.Object.ID, t.Subject.Relation); err != nil {
			return 0, fmt.Errorf("remove %v: %w", t, err)
		}
	}

	for _, t := range add {
		// ON CONFLICT DO NOTHING against the live-uniqueness index makes a
		// retried write idempotent: re-granting something already granted is a
		// no-op, not a duplicate row or an error.
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO tuples
                (object_type, object_id, relation,
                 subject_type, subject_id, subject_relation, created_rev)
            VALUES ($1,$2,$3,$4,$5,$6,$7)
            ON CONFLICT DO NOTHING`,
			t.Object.Type, t.Object.ID, t.Relation,
			t.Subject.Object.Type, t.Subject.Object.ID, t.Subject.Relation, rev); err != nil {
			return 0, fmt.Errorf("add %v: %w", t, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return rev, nil
}

func (p *Postgres) Read(ctx context.Context, rev Revision, object Object, relation string) ([]Tuple, error) {
	rows, err := p.db.QueryContext(ctx, `
        SELECT subject_type, subject_id, subject_relation
          FROM tuples
         WHERE object_type = $1 AND object_id = $2 AND relation = $3
           AND created_rev <= $4
           AND (deleted_rev = 0 OR deleted_rev > $4)`,
		object.Type, object.ID, relation, rev)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tuple
	for rows.Next() {
		var st, sid, srel string
		if err := rows.Scan(&st, &sid, &srel); err != nil {
			return nil, err
		}
		out = append(out, Tuple{
			Object:   object,
			Relation: relation,
			Subject:  Subject{Object: Object{Type: st, ID: sid}, Relation: srel},
		})
	}
	return out, rows.Err()
}

// ReadBySubject walks the reverse index: every object to which this subject is
// related. It is the primitive ListObjects is built from, and the reason
// idx_tuple_reverse exists. A relation of "" matches any relation.
func (p *Postgres) ReadBySubject(ctx context.Context, rev Revision, subject Subject, relation string) ([]Tuple, error) {
	rows, err := p.db.QueryContext(ctx, `
        SELECT object_type, object_id, relation
          FROM tuples
         WHERE subject_type = $1 AND subject_id = $2 AND subject_relation = $3
           AND ($4 = '' OR relation = $4)
           AND created_rev <= $5
           AND (deleted_rev = 0 OR deleted_rev > $5)`,
		subject.Object.Type, subject.Object.ID, subject.Relation, relation, rev)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tuple
	for rows.Next() {
		var ot, oid, rel string
		if err := rows.Scan(&ot, &oid, &rel); err != nil {
			return nil, err
		}
		out = append(out, Tuple{
			Object:   Object{Type: ot, ID: oid},
			Relation: rel,
			Subject:  subject,
		})
	}
	return out, rows.Err()
}
