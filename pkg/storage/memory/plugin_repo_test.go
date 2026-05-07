package memory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AiRanthem/ANA/pkg/manager/plugin"
)

func TestPluginRepository_RoundTrip(t *testing.T) {
	t.Parallel()

	repo := NewPluginRepository()
	now := time.Now().UTC()
	p := plugin.Plugin{
		ID:        "plg_1",
		Namespace: "default",
		Name:      "demo",
		Manifest: plugin.Manifest{
			SchemaVersion: 1,
			Plugin:        plugin.ManifestPlugin{Name: "demo"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	if err := repo.Insert(context.Background(), p); !errors.Is(err, plugin.ErrPluginNameConflict) {
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

	rows, next, err := repo.List(context.Background(), plugin.ListOptions{Limit: 10})
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
	if _, err := repo.Get(context.Background(), p.ID); !errors.Is(err, plugin.ErrPluginNotFound) {
		t.Fatalf("Get() after delete error = %v, want ErrPluginNotFound", err)
	}
}

func TestPluginRepository_NameIndexUsesStructuredKey(t *testing.T) {
	t.Parallel()

	repo := NewPluginRepository()
	now := time.Now().UTC()
	first := testPlugin("plg_first", "a", "b/c", now)
	second := testPlugin("plg_second", "a/b", "c", now.Add(time.Second))

	if err := repo.Insert(context.Background(), first); err != nil {
		t.Fatalf("Insert(first) error = %v", err)
	}
	if err := repo.Insert(context.Background(), second); err != nil {
		t.Fatalf("Insert(second) error = %v", err)
	}

	gotFirst, err := repo.GetByName(context.Background(), first.Namespace, first.Name)
	if err != nil {
		t.Fatalf("GetByName(first) error = %v", err)
	}
	if gotFirst.ID != first.ID {
		t.Fatalf("GetByName(first).ID = %q, want %q", gotFirst.ID, first.ID)
	}

	gotSecond, err := repo.GetByName(context.Background(), second.Namespace, second.Name)
	if err != nil {
		t.Fatalf("GetByName(second) error = %v", err)
	}
	if gotSecond.ID != second.ID {
		t.Fatalf("GetByName(second).ID = %q, want %q", gotSecond.ID, second.ID)
	}
}

func TestPluginRepository_List_MaxIntLimitWithNonZeroCursorDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("List() panicked: %v", r)
		}
	}()

	repo := NewPluginRepository()
	now := time.Now().UTC()
	for i, name := range []string{"alpha", "beta"} {
		p := testPlugin(plugin.PluginID("plg_"+name), "default", name, now.Add(time.Duration(i)*time.Second))
		if err := repo.Insert(context.Background(), p); err != nil {
			t.Fatalf("Insert() error = %v", err)
		}
	}

	maxInt := int(^uint(0) >> 1)
	rows, next, err := repo.List(context.Background(), plugin.ListOptions{Limit: maxInt, Cursor: "1"})
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

func TestPluginRepository_InsertDuplicateIDReturnsIDConflict(t *testing.T) {
	t.Parallel()

	repo := NewPluginRepository()
	now := time.Now().UTC()
	base := testPlugin("plg_dup_id", "default", "alpha", now)
	if err := repo.Insert(context.Background(), base); err != nil {
		t.Fatalf("Insert(base) error = %v", err)
	}

	conflict := base
	conflict.Name = "beta"
	conflict.Manifest.Plugin.Name = "beta"
	err := repo.Insert(context.Background(), conflict)
	if !errors.Is(err, plugin.ErrPluginIDConflict) {
		t.Fatalf("Insert(duplicate id) error = %v, want ErrPluginIDConflict", err)
	}
	if errors.Is(err, plugin.ErrPluginNameConflict) {
		t.Fatalf("Insert(duplicate id) error = %v, want no ErrPluginNameConflict", err)
	}
}

func TestPluginRepository_Get_DeepMetadataCloneDoesNotRecurseUnboundedly(t *testing.T) {
	t.Parallel()

	meta := nestPluginMetadataMaps(60)
	repo := NewPluginRepository()
	now := time.Now().UTC()
	p := testPlugin("plg_deep", "ns", "deep", now)
	p.Manifest.Plugin.Metadata = meta

	if err := repo.Insert(context.Background(), p); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := repo.Get(context.Background(), p.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

func TestPluginStorage_PutGetDelete(t *testing.T) {
	t.Parallel()

	st := NewPluginStorage()
	id := plugin.PluginID("plg_1")
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

	if _, err := st.PresignURL(context.Background(), id, plugin.PresignOptions{}); !errors.Is(err, plugin.ErrUnsupported) {
		t.Fatalf("PresignURL() error = %v, want ErrUnsupported", err)
	}

	if err := st.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, _, err := st.Get(context.Background(), id); !errors.Is(err, plugin.ErrStorageNotFound) {
		t.Fatalf("Get() after delete error = %v, want ErrStorageNotFound", err)
	}
}

func TestPluginStorage_Put_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	st := NewPluginStorage()
	over := bytes.Repeat([]byte("x"), int(memoryStorageMaxPutBodyBytes)+1)
	_, err := st.Put(context.Background(), "plg_x", bytes.NewReader(over))
	if err == nil {
		t.Fatal("Put: want error for oversized body")
	}
	if !errors.Is(err, plugin.ErrCorruptArchive) {
		t.Fatalf("Put error = %v, want ErrCorruptArchive", err)
	}
}

func TestPluginStorage_AtomicOverwrite(t *testing.T) {
	t.Parallel()

	st := NewPluginStorage()
	id := plugin.PluginID("plg_1")
	oldBody := []byte("old-body")
	newBody := []byte("new-body-new-body")

	if _, err := st.Put(context.Background(), id, bytes.NewReader(oldBody)); err != nil {
		t.Fatalf("Put old error = %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			rc, _, err := st.Get(context.Background(), id)
			if err != nil {
				errCh <- err
				return
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(b, oldBody) && !bytes.Equal(b, newBody) {
				errCh <- errors.New("observed mixed body")
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if _, err := st.Put(context.Background(), id, bytes.NewReader(newBody)); err != nil {
				errCh <- err
				return
			}
			if _, err := st.Put(context.Background(), id, bytes.NewReader(oldBody)); err != nil {
				errCh <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("atomic overwrite read error: %v", err)
		}
	}
}

func TestPluginStorage_List_Sorted(t *testing.T) {
	t.Parallel()

	st := NewPluginStorage()
	ids := []plugin.PluginID{"plg_c", "plg_a", "plg_b"}
	for _, id := range ids {
		if _, err := st.Put(context.Background(), id, bytes.NewReader([]byte(string(id)))); err != nil {
			t.Fatalf("Put(%s) error = %v", id, err)
		}
	}
	got, err := st.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []plugin.PluginID{"plg_a", "plg_b", "plg_c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
}

func testPlugin(id plugin.PluginID, namespace plugin.Namespace, name string, now time.Time) plugin.Plugin {
	return plugin.Plugin{
		ID:        id,
		Namespace: namespace,
		Name:      name,
		Manifest: plugin.Manifest{
			SchemaVersion: 1,
			Plugin:        plugin.ManifestPlugin{Name: name},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func nestPluginMetadataMaps(depth int) map[string]any {
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
