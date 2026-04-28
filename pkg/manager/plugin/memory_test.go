package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestMemoryRepository_RoundTrip(t *testing.T) {
	t.Parallel()

	repo := NewMemoryRepository()
	now := time.Now().UTC()
	p := Plugin{
		ID:        "plg_1",
		Namespace: "default",
		Name:      "demo",
		Manifest: Manifest{
			SchemaVersion: 1,
			Plugin:        ManifestPlugin{Name: "demo"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	if err := repo.Insert(context.Background(), p); !errors.Is(err, ErrPluginNameConflict) {
		t.Fatalf("duplicate Insert() error = %v, want ErrPluginNameConflict", err)
	}

	got, err := repo.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != p.Name {
		t.Fatalf("Get().Name = %q, want %q", got.Name, p.Name)
	}

	gotByName, err := repo.GetByName(context.Background(), p.Namespace, p.Name)
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}
	if gotByName.ID != p.ID {
		t.Fatalf("GetByName().ID = %q, want %q", gotByName.ID, p.ID)
	}

	rows, next, err := repo.List(context.Background(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 || next != "" {
		t.Fatalf("List() = (%d rows, next=%q), want (1, empty)", len(rows), next)
	}

	p.Description = "updated"
	p.UpdatedAt = now.Add(time.Second)
	if err := repo.Update(context.Background(), p); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	updated, err := repo.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Get() after update error = %v", err)
	}
	if updated.Description != "updated" {
		t.Fatalf("updated Description = %q", updated.Description)
	}

	if err := repo.Delete(context.Background(), p.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := repo.Get(context.Background(), p.ID); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("Get() after delete error = %v, want ErrPluginNotFound", err)
	}
}

func TestMemoryRepository_List_MaxIntLimitWithNonZeroCursorDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("List() panicked: %v", r)
		}
	}()

	repo := NewMemoryRepository()
	now := time.Now().UTC()
	for i, name := range []string{"alpha", "beta"} {
		p := Plugin{
			ID:        PluginID("plg_" + name),
			Namespace: "default",
			Name:      name,
			Manifest: Manifest{
				SchemaVersion: 1,
				Plugin:        ManifestPlugin{Name: name},
			},
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := repo.Insert(context.Background(), p); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	maxInt := int(^uint(0) >> 1)
	rows, next, err := repo.List(context.Background(), ListOptions{Limit: maxInt, Cursor: "1"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List() len = %d, want 1", len(rows))
	}
	if next != "" {
		t.Fatalf("List() next = %q, want empty", next)
	}
}

func TestMemoryRepository_InsertDuplicateIDReturnsIDConflict(t *testing.T) {
	t.Parallel()

	repo := NewMemoryRepository()
	now := time.Now().UTC()
	base := Plugin{
		ID:        "plg_dup_id",
		Namespace: "default",
		Name:      "alpha",
		Manifest: Manifest{
			SchemaVersion: 1,
			Plugin:        ManifestPlugin{Name: "alpha"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.Insert(context.Background(), base); err != nil {
		t.Fatalf("Insert(base) error = %v", err)
	}

	conflict := base
	conflict.Name = "beta"
	conflict.Manifest.Plugin.Name = "beta"
	err := repo.Insert(context.Background(), conflict)
	if !errors.Is(err, ErrPluginIDConflict) {
		t.Fatalf("Insert(duplicate id) error = %v, want ErrPluginIDConflict", err)
	}
	if errors.Is(err, ErrPluginNameConflict) {
		t.Fatalf("Insert(duplicate id) error = %v, want no ErrPluginNameConflict", err)
	}
}

func TestMemoryStorage_PutGetDelete(t *testing.T) {
	t.Parallel()

	st := NewMemoryStorage()
	id := PluginID("plg_1")
	body := []byte("plugin-zip-content")

	obj, err := st.Put(context.Background(), id, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if obj.Size != int64(len(body)) {
		t.Fatalf("Put().Size = %d, want %d", obj.Size, len(body))
	}

	rc, gotObj, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer rc.Close()

	gotBody, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("Get() body mismatch")
	}
	if gotObj.ContentHash != obj.ContentHash {
		t.Fatalf("Get() content hash mismatch")
	}

	if _, err := st.PresignURL(context.Background(), id, PresignOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("PresignURL() error = %v, want ErrUnsupported", err)
	}

	if err := st.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, _, err := st.Get(context.Background(), id); !errors.Is(err, ErrStorageNotFound) {
		t.Fatalf("Get() after delete error = %v, want ErrStorageNotFound", err)
	}
}

func TestMemoryStorage_AtomicOverwrite(t *testing.T) {
	t.Parallel()

	st := NewMemoryStorage()
	id := PluginID("plg_1")
	oldBody := []byte("old-body")
	newBody := []byte("new-body-new-body")

	if _, err := st.Put(context.Background(), id, bytes.NewReader(oldBody)); err != nil {
		t.Fatalf("Put old error = %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var readErr error
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			rc, _, err := st.Get(context.Background(), id)
			if err != nil {
				readErr = err
				return
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				readErr = err
				return
			}
			if !bytes.Equal(b, oldBody) && !bytes.Equal(b, newBody) {
				readErr = errors.New("observed mixed body")
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if _, err := st.Put(context.Background(), id, bytes.NewReader(newBody)); err != nil {
				readErr = err
				return
			}
			if _, err := st.Put(context.Background(), id, bytes.NewReader(oldBody)); err != nil {
				readErr = err
				return
			}
		}
	}()

	wg.Wait()
	if readErr != nil {
		t.Fatalf("atomic overwrite read error: %v", readErr)
	}
}

func TestMemoryStorage_List_Sorted(t *testing.T) {
	t.Parallel()
	st := NewMemoryStorage()
	ids := []PluginID{"plg_c", "plg_a", "plg_b"}
	for _, id := range ids {
		if _, err := st.Put(context.Background(), id, bytes.NewReader([]byte(string(id)))); err != nil {
			t.Fatalf("Put(%s) error = %v", id, err)
		}
	}
	got, err := st.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []PluginID{"plg_a", "plg_b", "plg_c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
}
