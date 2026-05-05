package memory

import (
	"bytes"
	"context"
	"fmt"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
	"io"
	"sort"
	"sync"
)

type memoryBlob struct {
	body   []byte
	object plugin.StoredObject
}

// MemoryStorage is a concurrent-safe in-memory Storage implementation.
type PluginStorage struct {
	mu     sync.RWMutex
	blobs  map[plugin.PluginID]memoryBlob
	closed bool
}

// memoryStorageMaxPutBodyBytes matches the default zip payload cap (see OpenZipReaderFromStream).
const memoryStorageMaxPutBodyBytes = int64(256 << 20)

func NewPluginStorage() *PluginStorage {
	return &PluginStorage{
		blobs: make(map[plugin.PluginID]memoryBlob),
	}
}

func (s *PluginStorage) Put(_ context.Context, id plugin.PluginID, body io.Reader) (plugin.StoredObject, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return plugin.StoredObject{}, plugin.ErrStorageClosed
	}
	s.mu.RUnlock()

	limited := &io.LimitedReader{R: body, N: memoryStorageMaxPutBodyBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return plugin.StoredObject{}, err
	}
	if int64(len(data)) > memoryStorageMaxPutBodyBytes {
		return plugin.StoredObject{}, fmt.Errorf("%w: plugin body exceeds %d bytes", plugin.ErrCorruptArchive, memoryStorageMaxPutBodyBytes)
	}
	hash, size, err := plugin.Hash(bytes.NewReader(data))
	if err != nil {
		return plugin.StoredObject{}, err
	}
	obj := plugin.StoredObject{
		Size:        size,
		ContentHash: hash,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return plugin.StoredObject{}, plugin.ErrStorageClosed
	}
	s.blobs[id] = memoryBlob{
		body:   bytes.Clone(data),
		object: obj,
	}
	return obj, nil
}

func (s *PluginStorage) Get(_ context.Context, id plugin.PluginID) (io.ReadCloser, plugin.StoredObject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, plugin.StoredObject{}, plugin.ErrStorageClosed
	}
	blob, ok := s.blobs[id]
	if !ok {
		return nil, plugin.StoredObject{}, plugin.ErrStorageNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(blob.body))), blob.object, nil
}

func (s *PluginStorage) Delete(_ context.Context, id plugin.PluginID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return plugin.ErrStorageClosed
	}
	delete(s.blobs, id)
	return nil
}

func (s *PluginStorage) PresignURL(_ context.Context, _ plugin.PluginID, _ plugin.PresignOptions) (string, error) {
	return "", plugin.ErrUnsupported
}

func (s *PluginStorage) List(_ context.Context) ([]plugin.PluginID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, plugin.ErrStorageClosed
	}
	out := make([]plugin.PluginID, 0, len(s.blobs))
	for id := range s.blobs {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		return string(out[i]) < string(out[j])
	})
	return out, nil
}

func (s *PluginStorage) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
