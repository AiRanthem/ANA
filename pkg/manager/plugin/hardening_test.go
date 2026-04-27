package plugin

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStorage_Put_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	s := NewMemoryStorage()
	over := bytes.Repeat([]byte("x"), int(memoryStorageMaxPutBodyBytes)+1)
	_, err := s.Put(context.Background(), "plg_x", bytes.NewReader(over))
	if err == nil {
		t.Fatal("Put: want error for oversized body")
	}
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("Put error = %v, want ErrCorruptArchive", err)
	}
}

func TestValidateManifest_MetadataExceedsNesting(t *testing.T) {
	t.Parallel()

	m := Manifest{
		SchemaVersion: 1,
		Plugin: ManifestPlugin{
			Name:     "demo",
			Metadata: nestMetadataMaps(80),
		},
	}
	err := ValidateManifest(m)
	if err == nil {
		t.Fatal("ValidateManifest: want error for deep metadata")
	}
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("error = %v, want ErrInvalidManifest", err)
	}
}

func nestMetadataMaps(depth int) map[string]any {
	root := map[string]any{}
	cur := root
	for i := 0; i < depth; i++ {
		next := map[string]any{}
		cur["k"] = next
		cur = next
	}
	cur["leaf"] = true
	return root
}

func TestMemoryRepository_Get_DeepMetadataCloneDoesNotRecurseUnboundedly(t *testing.T) {
	t.Parallel()

	meta := nestMetadataMaps(60)
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	p := Plugin{
		ID:        "plg_deep",
		Namespace: "ns",
		Name:      "deep",
		Manifest: Manifest{
			SchemaVersion: 1,
			Plugin: ManifestPlugin{
				Name:     "deep",
				Metadata: meta,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := repo.Get(context.Background(), p.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
}
