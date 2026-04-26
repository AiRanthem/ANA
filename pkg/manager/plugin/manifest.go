package plugin

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

const manifestSchemaV1 = 1

var pluginNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// ParseManifest decodes manifest.toml bytes and validates schema-v1 rules.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: parse toml: %v", ErrInvalidManifest, err)
	}
	if err := ValidateManifest(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// ValidateManifest validates schema-v1 constraints independent from archive IO.
func ValidateManifest(m Manifest) error {
	var errs []error

	if m.SchemaVersion != manifestSchemaV1 {
		errs = append(errs, fmt.Errorf("schema_version must be %d", manifestSchemaV1))
	}
	if !pluginNamePattern.MatchString(m.Plugin.Name) {
		errs = append(errs, errors.New("plugin.name must match [a-z0-9-]{1,64}"))
	}
	if len(m.Plugin.Description) > 1024 {
		errs = append(errs, errors.New("plugin.description exceeds 1024 chars"))
	}
	if err := validateMetadata(m.Plugin.Metadata, "plugin.metadata"); err != nil {
		errs = append(errs, err)
	}

	validateSection := func(section Section, entries map[string]ManifestEntry) {
		for key, entry := range entries {
			if strings.TrimSpace(key) == "" {
				errs = append(errs, fmt.Errorf("%s entry key is empty", section))
			}
			if strings.TrimSpace(entry.Path) == "" {
				errs = append(errs, fmt.Errorf("%s.%s.path is required", section, key))
				continue
			}
			if !isSafeArchivePath(entry.Path) {
				errs = append(errs, fmt.Errorf("%s.%s.path is invalid: %q", section, key, entry.Path))
			}
		}
	}

	validateSection(SectionSkills, m.Skills)
	validateSection(SectionRules, m.Rules)
	validateSection(SectionHooks, m.Hooks)
	validateSection(SectionSubagents, m.Subagents)
	validateSection(SectionMCPs, m.MCPs)

	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, errors.Join(errs...))
	}
	return nil
}

func validateMetadata(meta map[string]any, prefix string) error {
	for key, value := range meta {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s has an empty key", prefix)
		}
		if err := validateMetadataValue(value, prefix+"."+key); err != nil {
			return err
		}
	}
	return nil
}

func validateMetadataValue(value any, keyPath string) error {
	switch v := value.(type) {
	case nil, bool, string,
		int64, int32, int16, int8, int,
		uint64, uint32, uint16, uint8, uint,
		float32, float64:
		return nil
	case []any:
		for idx, item := range v {
			if err := validateScalar(item); err != nil {
				return fmt.Errorf("%s[%d]: %w", keyPath, idx, err)
			}
		}
		return nil
	case map[string]any:
		for k, item := range v {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("%s has nested empty key", keyPath)
			}
			if err := validateMetadataValue(item, keyPath+"."+k); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s has unsupported type %T", keyPath, value)
	}
}

func validateScalar(value any) error {
	switch value.(type) {
	case nil, bool, string,
		int64, int32, int16, int8, int,
		uint64, uint32, uint16, uint8, uint,
		float32, float64:
		return nil
	default:
		return fmt.Errorf("non-scalar array value type %T", value)
	}
}

func isSafeArchivePath(p string) bool {
	if p == "" || strings.Contains(p, "\\") {
		return false
	}
	if strings.HasPrefix(p, "/") {
		return false
	}
	// Reject dot-segments and empty segments in the raw path before path.Clean,
	// which collapses "skills/../x" into "skills/x" and would otherwise hide escapes.
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	clean := path.Clean(p)
	if clean == "." || clean == "/" {
		return false
	}
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return false
	}
	if strings.Contains("/"+clean+"/", "/../") {
		return false
	}
	return true
}
