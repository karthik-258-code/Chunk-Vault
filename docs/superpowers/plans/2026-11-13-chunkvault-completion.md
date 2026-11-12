# ChunkVault Completion Plan (M2b → v1.0.0)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take ChunkVault from its current state (M2b code-complete, unmarked) through M2c, M3, and M4 to a released v1.0.0.

**Architecture:** The codebase is clean layered Go: `internal/chunker` → `internal/codec` → `internal/store` → `internal/vault` → `cmd/chunkvault`. Each milestone adds a vertical slice without restructuring existing layers. GC operates on the index (zero-ref chunks) and the blob store. The S3 backend implements the existing `BlobStore` interface. The REST API wraps `vault.Vault` methods behind chi routes.

**Tech Stack:** Go 1.23, bbolt, zstd, AES-256-GCM, Argon2id, cobra, chi, AWS SDK v2 (S3), docker-compose, golangci-lint.

**Spec:** `README.md` roadmap section + this plan.

## Global Constraints

- Go 1.23 minimum; no new direct dependencies without justification.
- All new public functions have godoc comments.
- `make test` (go test -race ./...) must pass after every task.
- `make lint` must pass after every task.
- Commit after every task with `git commit -m "feat/fix/chore: ..."`.
- File names are `refs/<name>` on disk; names are not encrypted (known gap, not in scope here).

## Review Focus

1. **GC races with concurrent `put`**: a chunk written after GC starts its scan but before it deletes blobs gets incorrectly deleted. The GC must hold a lock or use a two-phase approach (scan → verify still zero-ref → delete).
2. **S3 `Put` partial failure**: if the multipart upload succeeds but the index update fails, the blob exists in S3 but the vault thinks the put failed. Idempotent `Has`+`Put` semantics must hold.
3. **Parallel `put` duplicate chunk writes**: two goroutines may both see `Has=false` for the same chunk and both write it; only the first `Put` must count as `created=true` for stats.
4. **REST API passphrase over HTTP**: the passphrase must never travel in a URL or query param; the design must enforce this in tests.
5. **Snapshot isolation**: a snapshot records the manifest keys at a point in time; a concurrent `put` or `rm` between snapshot creation and `get` must not corrupt the snapshot.

---

## YOUR ACTION STEPS (simple version)

| Step | What to say | Model |
|------|------------|-------|
| 1 | `fix readme m2b done` | Sonnet |
| 2 | `implement m2c parallel workers and benchmarks` | Sonnet |
| 3 | `implement m3 garbage collector` | Sonnet |
| 4 | `implement m3 s3 backend` | Sonnet |
| 5 | `implement m3 rest api and docker-compose` | **Opus** |
| 6 | `implement m4 snapshots` | **Opus** |
| 7 | `implement m4 final benchmarks docs and v1 release` | Sonnet |

> Switch to Opus for steps 5 and 6 only — the REST API design and snapshot isolation are the two hard architectural problems. Everything else Sonnet handles fine.

---

## Task 1: Mark M2b complete in README

**Files:**
- Modify: `README.md:231`

**Interfaces:**
- Produces: nothing (housekeeping)

- [ ] **Step 1:** In `README.md` change `- [ ] **M2b**` to `- [x] **M2b**`
- [ ] **Step 2:** Run `make test` — expect PASS
- [ ] **Step 3:** Commit

```bash
git add README.md
git commit -m "docs: mark M2b complete"
```

---

## Task 2: Parallel worker pool for `put` and `get`

**Files:**
- Modify: `internal/vault/vault.go` — `Put` and `Get` methods
- Create: `internal/vault/worker.go` — `workerPool` type, `parallelPut`, `parallelGet` helpers
- Modify: `internal/vault/vault_test.go` — add parallel throughput tests

**Interfaces:**
- Consumes: `vault.Vault`, `chunker.Next()`, `store.BlobStore`, `codec.Codec`
- Produces: no new public API; `Put` and `Get` behaviour unchanged, faster

- [ ] **Step 1: Write failing benchmark**

```go
// internal/vault/worker_bench_test.go
func BenchmarkPutParallel(b *testing.B) { /* put 64 MiB, workers=4 */ }
```

- [ ] **Step 2: Run benchmark to get baseline**

```
go test -bench=BenchmarkPutParallel ./internal/vault/ -benchtime=3s
```

- [ ] **Step 3: Implement `workerPool` in `internal/vault/worker.go`**

A fixed-size goroutine pool that accepts `func() error` jobs and collects the first error. `parallelPut(ctx, keys []chunkJob, workers int) error` fans out `putBlob` calls. Worker count defaults to `runtime.GOMAXPROCS(0)`, capped at 8. Chunk ordering in the manifest is preserved (jobs carry an index; results are collected into a pre-allocated slice).

- [ ] **Step 4: Wire `parallelPut` into `Vault.Put`** — replace the serial `putBlob` loop

- [ ] **Step 5: Wire `parallelGet` into `Vault.Get`** — prefetch up to `workers` chunks ahead using a semaphore, preserve write order

- [ ] **Step 6: Run benchmark, confirm improvement**

- [ ] **Step 7: Run `make test && make lint`** — expect PASS

- [ ] **Step 8: Commit**

```bash
git add internal/vault/worker.go internal/vault/vault.go internal/vault/worker_bench_test.go
git commit -m "perf: parallel worker pool for put and get"
```

---

## Task 3: End-to-end benchmarks

**Files:**
- Create: `cmd/chunkvault/bench_test.go` — end-to-end `put`/`get` benchmarks with real temp dirs

**Interfaces:**
- Consumes: `newRootCmd`, real filesystem via `os.MkdirTemp`
- Produces: `BenchmarkPutGetRoundtrip`, `BenchmarkPutDeduplicated` — runnable with `make bench`

- [ ] **Step 1: Write `BenchmarkPutGetRoundtrip`**

Creates a temp repo, generates 20 MiB of random data, `put`, `get`, measures total wall time and reports MB/s via `b.SetBytes`.

- [ ] **Step 2: Write `BenchmarkPutDeduplicated`**

Same 20 MiB file put twice; second `put` must report >99% dedup. Measure second `put` only.

- [ ] **Step 3: Add `bench-e2e` target to `Makefile`**

```makefile
bench-e2e:
	go test -bench=. -benchtime=3s ./cmd/chunkvault/
```

- [ ] **Step 4: Run `make bench-e2e`** — expect numbers printed, no errors

- [ ] **Step 5: Commit**

```bash
git add cmd/chunkvault/bench_test.go Makefile
git commit -m "bench: end-to-end put/get benchmarks"
```

---

## Task 4: SAST / lint cleanup

**Files:**
- Modify: any files flagged by `make lint` or `.gitlab-ci.yml` SAST

**Interfaces:**
- Produces: clean `make lint` with zero findings

- [ ] **Step 1:** Run `make lint`; collect all findings
- [ ] **Step 2:** Fix each finding; do not suppress with `//nolint` unless the linter is wrong — add a comment explaining why
- [ ] **Step 3:** Run `make lint` again — expect zero findings
- [ ] **Step 4:** Run `make test` — expect PASS
- [ ] **Step 5:** Commit

```bash
git add -u
git commit -m "chore: resolve lint and SAST findings"
```

---

## Task 5: Mark M2c complete in README

- [ ] Change `- [ ] **M2c**` to `- [x] **M2c**` in `README.md`
- [ ] `git commit -m "docs: mark M2c complete"`

---

## Task 6: Mark-and-sweep garbage collector (`gc` command)

**Files:**
- Create: `internal/vault/gc.go` — `GC(ctx, dryRun bool) (GCStats, error)` on `*Vault`
- Create: `internal/vault/gc_test.go`
- Modify: `cmd/chunkvault/commands.go` — `newGCCmd`
- Modify: `README.md` — add `gc` to quickstart

**Interfaces:**
- Consumes: `v.idx.ForEachChunk`, `v.blobs.Delete(ctx, key)`, `v.idx.ForgetChunk(key)`
- Produces: `vault.GCStats{Collected int64, FreedBytes int64}`, `vault.GC(ctx, dryRun bool) (GCStats, error)`

The GC algorithm is two-phase to avoid the race (see Review Focus #1):

**Phase 1 (scan):** collect all zero-ref chunk keys from the index into a list. This is a snapshot — new zero-ref chunks that appear after this point are not in the list.

**Phase 2 (delete):** for each key in the list, re-read the index entry; if `Refs > 0`, skip (a concurrent `put` added a ref). Otherwise `blobs.Delete` then `idx.ForgetChunk`.

- [ ] **Step 1: Write failing test for GC**

```go
// gc_test.go
func TestGCDeletesUnreferencedChunks(t *testing.T) {
    // put file A, rm file A, run GC, verify blobs.List() is empty
}
func TestGCSkipsReferencedChunks(t *testing.T) {
    // put file A and file B sharing chunks, rm file A, run GC,
    // verify file B still restores correctly
}
func TestGCDryRun(t *testing.T) {
    // dry run must not delete any blobs
}
```

- [ ] **Step 2: Run tests** — expect FAIL (GC not implemented)

- [ ] **Step 3: Implement `GC` in `internal/vault/gc.go`**

Signature: `func (v *Vault) GC(ctx context.Context, dryRun bool) (GCStats, error)`

Two-phase approach as described above. `dryRun=true` skips the delete calls, counts what would be freed.

- [ ] **Step 4: Run tests** — expect PASS

- [ ] **Step 5: Add `newGCCmd` to `cmd/chunkvault/commands.go`**

```
chunkvault gc [--dry-run] [--repo <dir>]
```

Output: `Collected N chunks, freed X MiB` (or `Would collect...` for dry-run).

- [ ] **Step 6: Add end-to-end test for `gc` command to `commands_test.go`**

- [ ] **Step 7: Run `make test && make lint`** — expect PASS

- [ ] **Step 8: Commit**

```bash
git add internal/vault/gc.go internal/vault/gc_test.go cmd/chunkvault/commands.go README.md
git commit -m "feat: mark-and-sweep garbage collector (gc command)"
```

---

## Task 7: S3/MinIO BlobStore backend

**Files:**
- Create: `internal/store/s3.go` — `S3Store` implementing `BlobStore`
- Create: `internal/store/s3_test.go` — integration tests (require `CHUNKVAULT_TEST_S3_*` env vars; skip if unset)
- Modify: `cmd/chunkvault/commands.go` — add `--s3-bucket`, `--s3-region`, `--s3-endpoint` flags to all commands
- Modify: `go.mod` — add `github.com/aws/aws-sdk-go-v2`

**Interfaces:**
- Consumes: `store.BlobStore` interface (`Put`, `Get`, `Has`, `Delete`, `List`, `Size`)
- Produces: `store.NewS3(ctx, bucket, region, endpoint string) (*S3Store, error)`

```go
// BlobStore interface (already defined in store/store.go)
type BlobStore interface {
    Put(ctx context.Context, key Key, r io.Reader) (created bool, err error)
    Get(ctx context.Context, key Key) (io.ReadCloser, error)
    Has(ctx context.Context, key Key) (bool, error)
    Size(ctx context.Context, key Key) (int64, error)
    Delete(ctx context.Context, key Key) error
    List(ctx context.Context) ([]Key, error)
}
```

- [ ] **Step 1: Add aws-sdk-go-v2 dependencies**

```bash
go get github.com/aws/aws-sdk-go-v2/config
go get github.com/aws/aws-sdk-go-v2/service/s3
go mod tidy
```

- [ ] **Step 2: Write skippable integration test**

```go
func TestS3RoundTrip(t *testing.T) {
    bucket := os.Getenv("CHUNKVAULT_TEST_S3_BUCKET")
    if bucket == "" { t.Skip("CHUNKVAULT_TEST_S3_BUCKET not set") }
    // put, get, size, delete
}
```

- [ ] **Step 3: Implement `S3Store` in `internal/store/s3.go`**

Key-to-object-name: `hex(key[:2]) + "/" + hex(key[2:4]) + "/" + hex(key)` (same fan-out as `fs.go`). `Put` uses S3 PutObject with the reader. `Has` uses HeadObject. `Size` uses HeadObject ContentLength. `Get` uses GetObject. `Delete` uses DeleteObject. `List` uses ListObjectsV2 with prefix pagination.

- [ ] **Step 4: Wire flags into all commands in `commands.go`**

If `--s3-bucket` is set, construct `S3Store` and pass to `vault.New`; otherwise use `store.NewFS`.

- [ ] **Step 5: Run integration test against MinIO (local docker)**

```bash
docker run -d -p 9000:9000 minio/minio server /data
CHUNKVAULT_TEST_S3_BUCKET=test CHUNKVAULT_TEST_S3_ENDPOINT=http://localhost:9000 go test ./internal/store/ -run TestS3
```

- [ ] **Step 6: Run `make test && make lint`** — expect PASS

- [ ] **Step 7: Commit**

```bash
git add internal/store/s3.go internal/store/s3_test.go cmd/chunkvault/commands.go go.mod go.sum
git commit -m "feat: S3/MinIO blob store backend"
```

---

## Task 8: REST API (chi) + docker-compose

**Files:**
- Create: `internal/api/api.go` — chi router, handlers for all vault operations
- Create: `internal/api/api_test.go` — httptest-based tests for every endpoint
- Create: `cmd/chunkvaultd/main.go` — daemon entry point
- Create: `docker-compose.yml`
- Modify: `Makefile` — add `build-daemon`, `docker-up` targets

**Interfaces:**
- Consumes: `vault.Vault` (Put, Get, List, Remove, GC, Stats, Verify)
- Produces:

```
POST   /v1/files/{name}        — put (body = file data)
GET    /v1/files/{name}        — get (body = file data)
DELETE /v1/files/{name}        — rm
GET    /v1/files               — ls (JSON)
GET    /v1/stats               — stats (JSON)
POST   /v1/gc?dry_run=true     — gc
POST   /v1/verify?full=true    — verify (JSON report)
```

Passphrase: via `X-Chunkvault-Passphrase` header **only** (never URL/query). Tests must verify a request with the passphrase in a query param is rejected 400.

- [ ] **Step 1: Write failing API tests**

One test per endpoint using `httptest.NewServer`. Cover: round-trip put→get, 404 on missing file, passphrase-in-query-param rejected.

- [ ] **Step 2: Run tests** — expect FAIL

- [ ] **Step 3: Implement `internal/api/api.go`**

`NewRouter(v *vault.Vault) http.Handler` returns a chi router with all routes above. Stream `put` body directly to `v.Put`; stream `get` response directly from `v.Get`. No buffering of file data in memory.

- [ ] **Step 4: Run tests** — expect PASS

- [ ] **Step 5: Implement `cmd/chunkvaultd/main.go`**

Flags: `--repo`, `--addr` (default `:8080`), `--s3-bucket` etc. Opens vault, starts HTTP server.

- [ ] **Step 6: Write `docker-compose.yml`**

Services: `chunkvaultd` (built from repo) + `minio`. Chunkvaultd uses S3 backend pointing at minio.

- [ ] **Step 7: Run `docker-compose up`, smoke test with curl**

```bash
curl -X POST http://localhost:8080/v1/files/test.txt --data-binary @README.md
curl http://localhost:8080/v1/files/test.txt | head
```

- [ ] **Step 8: Run `make test && make lint`** — expect PASS

- [ ] **Step 9: Commit**

```bash
git add internal/api/ cmd/chunkvaultd/ docker-compose.yml Makefile
git commit -m "feat: REST API (chi) and docker-compose"
```

---

## Task 9: Mark M3 complete in README

- [ ] Change `- [ ] **M3**` to `- [x] **M3**` in `README.md`
- [ ] `git commit -m "docs: mark M3 complete"`

---

## Task 10: Snapshots

**Files:**
- Create: `internal/vault/snapshot.go` — `Snapshot`, `CreateSnapshot`, `ListSnapshots`, `RestoreFromSnapshot`
- Create: `internal/vault/snapshot_test.go`
- Modify: `cmd/chunkvault/commands.go` — `snapshot create`, `snapshot ls`, `snapshot restore`
- Modify: `README.md`

**Interfaces:**
- Produces:

```go
type Snapshot struct {
    Name    string
    Tag     string    // user-provided label
    Created time.Time
    Files   []SnapshotEntry // {Name, ManifestKey}
}

func (v *Vault) CreateSnapshot(ctx context.Context, tag string) (Snapshot, error)
func (v *Vault) ListSnapshots(ctx context.Context) ([]Snapshot, error)
func (v *Vault) RestoreFromSnapshot(ctx context.Context, tag string) error
```

A snapshot is a manifest-of-manifests: a JSON blob stored in the blob store under a key derived from its content, with a ref in `refs/.snapshots/<tag>`. `RestoreFromSnapshot` does not delete files added after the snapshot; it only restores files whose manifest key differs from the snapshot's record. Files not in the snapshot are left untouched (not deleted) — this is a restore, not a rollback. Document this clearly.

**Concurrency note:** `CreateSnapshot` reads refs under no lock. A concurrent `put` or `rm` between the read and the manifest-store is acceptable — the snapshot reflects the state at the moment refs were read, which is a valid consistent point.

- [ ] **Step 1: Write failing tests**

```go
func TestSnapshotRoundTrip(t *testing.T)       // create, list, restore
func TestSnapshotIsolation(t *testing.T)       // put after snapshot, restore, old version back
func TestSnapshotDoesNotDeleteNewFiles(t *testing.T) // file added after snapshot survives restore
```

- [ ] **Step 2: Run tests** — expect FAIL

- [ ] **Step 3: Implement `internal/vault/snapshot.go`**

- [ ] **Step 4: Run tests** — expect PASS

- [ ] **Step 5: Add `snapshot` subcommands to CLI**

- [ ] **Step 6: Run `make test && make lint`** — PASS

- [ ] **Step 7: Commit**

```bash
git add internal/vault/snapshot.go internal/vault/snapshot_test.go cmd/chunkvault/commands.go README.md
git commit -m "feat: snapshots"
```

---

## Task 11: Real-dataset benchmarks + design doc + v1.0.0 release

**Files:**
- Create: `docs/design.md` — architecture, encryption design, threat model, benchmark results
- Modify: `README.md` — populate benchmark results table with real numbers, mark M4 complete
- Modify: `.gitlab-ci.yml` — add `goreleaser` or `git tag v1.0.0` release job
- Modify: `cmd/chunkvault/main.go` — set `version = "1.0.0"`

- [ ] **Step 1: Run benchmarks on a real dataset** (Linux ISO or similar, 500 MB+)

```bash
chunkvault put archlinux.iso
# modify 1 MB in the middle
chunkvault put archlinux-modified.iso --name archlinux.iso
chunkvault stats
```

Record: dedup ratio, compression ratio, put throughput, get throughput.

- [ ] **Step 2: Write `docs/design.md`**

Sections: Overview, Architecture (the mermaid diagrams from README), Blob format, Encryption, Key derivation, Threat model, Benchmarks (results from step 1).

- [ ] **Step 3: Update README benchmark table with real numbers**

- [ ] **Step 4: Set `version = "1.0.0"` in `cmd/chunkvault/main.go`**

- [ ] **Step 5: Mark M4 complete in README**

- [ ] **Step 6: Run `make test && make lint`** — PASS

- [ ] **Step 7: Tag and release**

```bash
git tag v1.0.0
git push origin v1.0.0
```

- [ ] **Step 8: Commit**

```bash
git add docs/design.md README.md cmd/chunkvault/main.go
git commit -m "release: v1.0.0"
```

---

## Summary

| Task | Milestone | Model | Say |
|------|-----------|-------|-----|
| 1 — Mark M2b done | M2b | Sonnet | `fix readme m2b done` |
| 2 — Parallel workers | M2c | Sonnet | `implement m2c parallel workers` |
| 3 — E2E benchmarks | M2c | Sonnet | `implement m2c benchmarks` |
| 4 — SAST/lint | M2c | Sonnet | `implement m2c lint cleanup` |
| 5 — Mark M2c done | M2c | Sonnet | `mark m2c done in readme` |
| 6 — GC | M3 | Sonnet | `implement m3 garbage collector` |
| 7 — S3 backend | M3 | Sonnet | `implement m3 s3 backend` |
| 8 — REST API + docker | M3 | **Opus** | `implement m3 rest api and docker-compose` |
| 9 — Mark M3 done | M3 | Sonnet | `mark m3 done in readme` |
| 10 — Snapshots | M4 | **Opus** | `implement m4 snapshots` |
| 11 — Benchmarks + docs + release | M4 | Sonnet | `implement m4 release` |
