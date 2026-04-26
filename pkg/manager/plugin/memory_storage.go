package plugin

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
)

type memoryBlob struct {
	body   []byte
	object StoredObject
}

// MemoryStorage is a concurrent-safe in-memory Storage implementation.
type MemoryStorage struct {
	mu     sync.RWMutex
	blobs  map[PluginID]memoryBlob
	closed bool
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		blobs: make(map[PluginID]memoryBlob),
	}
}

func (s *MemoryStorage) Put(_ context.Context, id PluginID, body io.Reader) (StoredObject, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return StoredObject{}, ErrStorageClosed
	}
	s.mu.RUnlock()

	data, err := io.ReadAll(body)
	if err != nil {
		return StoredObject{}, err
	}
	hash, size, err := Hash(bytes.NewReader(data))
	if err != nil {
		return StoredObject{}, err
	}
	obj := StoredObject{
		Size:        size,
		ContentHash: hash,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return StoredObject{}, ErrStorageClosed
	}
	s.blobs[id] = memoryBlob{
		body:   bytes.Clone(data),
		object: obj,
	}
	return obj, nil
}

func (s *MemoryStorage) Get(_ context.Context, id PluginID) (io.ReadCloser, StoredObject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, StoredObject{}, ErrStorageClosed
	}
	blob, ok := s.blobs[id]
	if !ok {
		return nil, StoredObject{}, ErrStorageNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(blob.body))), blob.object, nil
}

func (s *MemoryStorage) Delete(_ context.Context, id PluginID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStorageClosed
	}
	delete(s.blobs, id)
	return nil
}

func (s *MemoryStorage) PresignURL(_ context.Context, _ PluginID, _ PresignOptions) (string, error) {
	return "", ErrUnsupported
}

func (s *MemoryStorage) List(_ context.Context) ([]PluginID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStorageClosed
	}
	out := make([]PluginID, 0, len(s.blobs))
	for id := range s.blobs {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		return string(out[i]) < string(out[j])
	})
	return out, nil
}

func (s *MemoryStorage) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
