package vault

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/karthik-258-code/Chunk-Vault/internal/store"
)

func TestSnapshotRoundTrip(t *testing.T) {
	v, _ := newVault(t)
	ctx := context.Background()

	data := randomBytes(1, 20_000)
	mustPut(t, v, "a.bin", data)
	mustPut(t, v, "b.bin", randomBytes(2, 10_000))

	snap, err := v.CreateSnapshot(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Tag != "v1" {
		t.Errorf("Tag = %q, want v1", snap.Tag)
	}
	if len(snap.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(snap.Files))
	}

	snaps, err := v.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Tag != "v1" {
		t.Fatalf("ListSnapshots = %+v", snaps)
	}

	// Restore when nothing changed is a no-op.
	if err := v.RestoreFromSnapshot(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, v, "a.bin"); !bytes.Equal(got, data) {
		t.Fatal("a.bin differs after restore with no changes")
	}
}

func TestSnapshotIsolation(t *testing.T) {
	v, _ := newVault(t)
	ctx := context.Background()

	original := randomBytes(10, 20_000)
	mustPut(t, v, "file.bin", original)

	if _, err := v.CreateSnapshot(ctx, "before-update"); err != nil {
		t.Fatal(err)
	}

	updated := randomBytes(11, 20_000)
	mustPut(t, v, "file.bin", updated)
	if !bytes.Equal(mustGet(t, v, "file.bin"), updated) {
		t.Fatal("expected updated content after second put")
	}

	if err := v.RestoreFromSnapshot(ctx, "before-update"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, v, "file.bin"); !bytes.Equal(got, original) {
		t.Fatal("file.bin did not revert to original after restore")
	}
}

func TestSnapshotDoesNotDeleteNewFiles(t *testing.T) {
	v, _ := newVault(t)
	ctx := context.Background()

	mustPut(t, v, "old.bin", randomBytes(20, 10_000))
	if _, err := v.CreateSnapshot(ctx, "snap"); err != nil {
		t.Fatal(err)
	}

	newData := randomBytes(21, 8_000)
	mustPut(t, v, "new.bin", newData)

	if err := v.RestoreFromSnapshot(ctx, "snap"); err != nil {
		t.Fatal(err)
	}

	// new.bin was not in the snapshot but must still be readable.
	if got := mustGet(t, v, "new.bin"); !bytes.Equal(got, newData) {
		t.Fatal("new.bin was removed by restore, but files outside the snapshot must not be deleted")
	}
}

func TestSnapshotTagValidation(t *testing.T) {
	v, _ := newVault(t)
	_, err := v.CreateSnapshot(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty tag")
	}
}

func TestSnapshotNotFound(t *testing.T) {
	v, _ := newVault(t)
	err := v.RestoreFromSnapshot(context.Background(), "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound, got: %v", err)
	}
}

func TestListSnapshotsEmpty(t *testing.T) {
	v, _ := newVault(t)
	snaps, err := v.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("expected 0 snapshots, got %d", len(snaps))
	}
}

func TestMultipleSnapshots(t *testing.T) {
	v, _ := newVault(t)
	ctx := context.Background()

	for _, tag := range []string{"alpha", "beta", "gamma"} {
		mustPut(t, v, tag+".bin", randomBytes(30, 5_000))
		if _, err := v.CreateSnapshot(ctx, tag); err != nil {
			t.Fatal(err)
		}
	}

	snaps, err := v.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snaps))
	}
	// Tags are returned in ascending (sorted) order.
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if snaps[i].Tag != want {
			t.Errorf("snaps[%d].Tag = %q, want %q", i, snaps[i].Tag, want)
		}
	}
}

func TestSnapshotEncryptedRepo(t *testing.T) {
	v, _ := initEncrypted(t)
	ctx := context.Background()

	data := randomBytes(40, 20_000)
	mustPut(t, v, "enc.bin", data)

	if _, err := v.CreateSnapshot(ctx, "enc-snap"); err != nil {
		t.Fatal(err)
	}

	snaps, err := v.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Tag != "enc-snap" {
		t.Fatalf("ListSnapshots = %+v", snaps)
	}

	mustPut(t, v, "enc.bin", randomBytes(41, 20_000))

	if err := v.RestoreFromSnapshot(ctx, "enc-snap"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, v, "enc.bin"); !bytes.Equal(got, data) {
		t.Fatal("encrypted restore: content mismatch")
	}
}
