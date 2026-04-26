package plugin

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// Cross-module identifiers are kept local so this package stays decoupled.
type (
	PluginID  string
	Namespace string
)

// Plugin is the plugin metadata row stored by Repository.
type Plugin struct {
	ID          PluginID
	Namespace   Namespace
	Name        string
	Description string
	Manifest    Manifest
	ContentHash string
	Size        int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Section is a canonical manifest section name.
type Section string

const (
	SectionSkills    Section = "skills"
	SectionRules     Section = "rules"
	SectionHooks     Section = "hooks"
	SectionSubagents Section = "subagents"
	SectionMCPs      Section = "mcps"
)

// Manifest is the schema-v1 manifest shape.
type Manifest struct {
	SchemaVersion int                      `toml:"schema_version"`
	Plugin        ManifestPlugin           `toml:"plugin"`
	Skills        map[string]ManifestEntry `toml:"skills,omitempty"`
	Rules         map[string]ManifestEntry `toml:"rules,omitempty"`
	Hooks         map[string]ManifestEntry `toml:"hooks,omitempty"`
	Subagents     map[string]ManifestEntry `toml:"subagents,omitempty"`
	MCPs          map[string]ManifestEntry `toml:"mcps,omitempty"`
}

// ManifestPlugin is the [plugin] table in manifest.toml.
type ManifestPlugin struct {
	Name        string         `toml:"name"`
	Description string         `toml:"description,omitempty"`
	Metadata    map[string]any `toml:"metadata,omitempty"`
}

// ManifestEntry is one item under a section table.
type ManifestEntry struct {
	Description string `toml:"description,omitempty"`
	DisplayName string `toml:"display_name,omitempty"`
	Path        string `toml:"path"`
}

// Reader exposes an unpacked plugin package.
type Reader interface {
	Manifest() Manifest
	FS() fs.FS
	Close() error
}

// Repository persists plugin metadata.
type Repository interface {
	Insert(ctx context.Context, p Plugin) error
	Update(ctx context.Context, p Plugin) error
	Get(ctx context.Context, id PluginID) (Plugin, error)
	GetByName(ctx context.Context, namespace Namespace, name string) (Plugin, error)
	List(ctx context.Context, opts ListOptions) ([]Plugin, string, error)
	Delete(ctx context.Context, id PluginID) error
	Close(ctx context.Context) error
}

// ListOptions controls repository listing.
type ListOptions struct {
	Namespace Namespace
	NameLike  string
	Limit     int
	Cursor    string
}

// Storage stores plugin zip blobs.
type Storage interface {
	Put(ctx context.Context, id PluginID, body io.Reader) (StoredObject, error)
	Get(ctx context.Context, id PluginID) (io.ReadCloser, StoredObject, error)
	Delete(ctx context.Context, id PluginID) error
	PresignURL(ctx context.Context, id PluginID, opts PresignOptions) (string, error)
	List(ctx context.Context) ([]PluginID, error)
	Close(ctx context.Context) error
}

// StoredObject describes a stored blob.
type StoredObject struct {
	Size        int64
	ContentHash string
}

// PresignOptions controls URL pre-signing behavior.
type PresignOptions struct {
	TTL    time.Duration
	Method string
}

var (
	ErrPluginNotFound     = errors.New("plugin: plugin not found")
	ErrPluginNameConflict = errors.New("plugin: plugin name conflict")
	ErrInvalidManifest    = errors.New("plugin: invalid manifest")
	ErrCorruptArchive     = errors.New("plugin: corrupt archive")
	ErrStorageClosed      = errors.New("plugin: storage closed")
	ErrStorageNotFound    = errors.New("plugin: storage not found")
	ErrUnsupported        = errors.New("plugin: unsupported")
)
