# zanzo

A relationship-based authorization service, in the shape of [Google's Zanzibar](https://research.google/pubs/pub48190/) — the system behind Drive, YouTube and Cloud IAM permissions.

Permissions are not stored. Relationships are:

```
document:readme  #parent  folder:eng
folder:eng       #viewer  group:eng#member
group:eng        #member  group:backend#member
group:backend    #member  user:riya
```

Nobody wrote "riya can view readme" anywhere. The schema says how to walk those edges to derive it, so a permission check is a reachability query over a graph, with a latency budget.

```
$ curl -s localhost:8080/v1/check -d '{
    "object":"document:readme","relation":"view","subject":"user:riya"}'
{"allowed":true,"token":"zk-4","expansions":8,"cache_hits":0,"max_depth":3}
```

## Why this is harder than it looks

Three problems make authorization different from ordinary CRUD.

**Caching a decision is dangerous.** A stale product listing shows an old price. A stale permission shows someone a document they were removed from ten seconds ago. Zanzibar's answer is that a cached entry carries the revision it was computed at, so it can never outlive the state that produced it — nothing is ever invalidated, entries for old revisions just stop being asked for.

**Depth is unbounded.** Groups nest, folders nest, and `parent->view` can chain arbitrarily. The engine carries a visited set for cycles and a hard depth limit, and denies rather than recursing when it hits either.

**The reverse question is a different problem.** "May Riya view this?" short-circuits on the first grant. "Which documents may Riya view?" has no starting object, cannot short-circuit, and its answer set is unbounded.

## Schema

```
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
}
```

A `relation` stores tuples. A `permission` is computed and stores none — writing a tuple against one would let a caller grant themselves something the schema says must be derived.

Operators are `+` union, `&` intersection, `-` exclusion, and `->` to walk an edge and evaluate a relation on the far side. **`+` and `-` share the loosest precedence and associate left**, so `a + b - c` is `(a + b) - c`. That is a security property, not formatting: `a + (b - c)` would let the left branch re-grant what the ban was meant to remove. There is a test pinning it.

## API

| Endpoint | Purpose |
|---|---|
| `POST /v1/check` | May this subject do this to this object? |
| `POST /v1/write` | Add/remove relationships; returns a consistency token |
| `POST /v1/list-objects` | Which objects of a type does this subject reach? |
| `POST /v1/expand` | The userset tree behind a relation, for audits and UIs |
| `GET /healthz`, `GET /stats` | Liveness and a server-side latency histogram |

### Consistency

Every response carries the token for the snapshot it was answered at. Writes return the token for the revision they created.

- `minimize_latency` (default) — read at current head, may serve from cache
- `at_least_as_fresh` — answer no earlier than the caller's token
- `full` — always read at head

`at_least_as_fresh` is what closes the **new-enemy** hole. A client that has just revoked access presents the token from that write, and the engine may not answer from anything older.

## The new-enemy problem

Remove Mallory from `folder:eng`. *Then* move `document:secret` into that folder. Both writes are individually fine. The bug is a read answered from a snapshot older than the removal — the "new enemy" sees the new document through an ACL that no longer exists.

The regression test deliberately includes the failure:

```
TestNaiveCacheExhibitsNewEnemy          PASS
    naive cache: mallory can read document:secret after being revoked (the bug)
TestRevisionKeyedCachePreventsNewEnemy  PASS
TestUncachedIsCorrect                   PASS
```

If the naive cache ever stops leaking, the scenario has stopped exercising the bug and the passing results below it would be worthless evidence. A separate test asserts the safe cache **actually hits** across requests — a cache that never hits is trivially correct and useless.

## ListObjects

Candidate generation, then verification:

1. Walk the reverse index outward from the subject, collecting objects that *might* qualify. Deliberately over-inclusive.
2. `Check` each candidate properly.

Step 2 is not laziness. Exclusion and intersection cannot be evaluated while walking backwards — reaching a document through its folder tells you nothing about whether the subject is banned on it, and the ban lives on the object you have only just discovered. Verifying with the real engine keeps one definition of "allowed" instead of two that drift apart.

The property test brute-forces `Check` over every object and demands an exact match, across 25 seeds × 12 users × 60 documents:

```
agreement held: 1758 grants, 44 candidates rejected by verification
```

Both numbers are asserted. Two empty sets also "agree", so the test fails if the generator stops producing enough access, or if the reverse walk stops over-proposing and leaves the verify step untested.

## Storage

Tuples carry the revision that created them and the revision that removed them, so a read at an older revision sees the world as it was and history is never destroyed. Writes are append-and-tombstone.

Two indexes serve opposite directions:

| Index | Serves |
|---|---|
| `(object_type, object_id, relation)` | `Check` — who is related to this object |
| `(subject_type, subject_id, subject_relation)` | `ListObjects` — what is this subject related to |

The reverse index is pure write amplification for Check-only traffic. That is the trade Zanzibar makes with its Leopard index, made explicit here.

A partial unique index covers only live rows, so a relationship can be granted, revoked and granted again — but never held twice at once.

One conformance suite runs against every `Store`. "It compiles against the interface" does not establish interchangeability: revision visibility, tombstoning and write idempotence are all invisible to the type system.

## Benchmarks

PostgreSQL 16.6, 45,300 tuples (20k documents, 500 folders, 400 group chains nested 3 deep, 20k users). 50 connections, 15s, rotating over distinct `(document, user)` pairs. Single node, consumer laptop.

| Config | rps | p50 | p99 |
|---|---|---|---|
| cache on, 5,000 pairs | 24,612 | 1ms | **5ms** |
| cache on, 20,000 pairs | 22,942 | 1ms | **8ms** |
| cache off, 20,000 pairs | 6,087 | 7ms | 21ms |

Sub-10ms p99 holds at a working set four times the cache-friendly one.

**Why the cache helps is worth stating precisely**, because the obvious reading is wrong. It is not that the same question repeats — 20,000 distinct pairs share almost no whole questions. They share *subproblems*: the folder and the group chain behind them. That is what the cache holds, and it is why quadrupling the working set costs only 5ms → 8ms.

## Known limits

Written down rather than discovered by whoever runs it next.

- **Single node.** The cache is in-process. A fleet would need consistent hashing to keep subproblems on the node that already has them; there is no cross-node coordination today.
- **ListObjects explores at most `MaxDepth` reverse edges.** A grant buried deeper is missed. `Check` has the same bound, so the two agree — but they agree on being wrong at the same point.
- **No type enforcement on writes.** The DSL parses `relation viewer: user | group#member`, and the parser keeps the restriction, but nothing rejects a tuple whose subject type the schema never allowed.
- **Revisions are a single counter.** Fine for one Postgres primary. Sharding the tuple store means revisions stop being globally ordered, which is where the design would need real work.
- **No pagination on ListObjects.** There is a limit, but no cursor, so a caller cannot walk a large result set.
- **`/stats` keeps every sample in memory.** It is a benchmarking aid, not production telemetry; Prometheus histograms belong there instead.

## Running it

```bash
go test ./...                       # memory store only
ZANZO_TEST_POSTGRES="postgres://postgres@127.0.0.1:5433/zanzo?sslmode=disable" go test ./...

go run ./cmd/zanzo -schema testdata/document.zanzo            # in-memory
ZANZO_POSTGRES="postgres://..." go run ./cmd/zanzo -schema testdata/document.zanzo
```

The Postgres test run isolates itself into a `zanzo_test` schema. An earlier version truncated whatever the DSN pointed at and quietly wiped a 45,300-tuple benchmark dataset mid-run — the tests passed, and the data was gone.
