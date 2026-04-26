package plugin

import (
	"errors"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	t.Parallel()

	valid := []byte(`
schema_version = 1
[plugin]
name = "trading-research"
description = "Stock research"
[plugin.metadata]
author = "ANA"
tags = ["finance", "research"]
[skills.stock_lookup]
display_name = "Stock lookup"
path = "skills/stock_lookup"
[rules.style]
description = "rules"
path = "rules/style.mdc"
`)

	m, err := ParseManifest(valid)
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	if m.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", m.SchemaVersion)
	}
	if got := m.Plugin.Name; got != "trading-research" {
		t.Fatalf("plugin.name = %q", got)
	}
}

func TestValidateManifest_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    Manifest
		want string
	}{
		{
			name: "wrong schema version",
			m: Manifest{
				SchemaVersion: 2,
				Plugin:        ManifestPlugin{Name: "abc"},
			},
			want: "schema_version",
		},
		{
			name: "empty plugin name",
			m: Manifest{
				SchemaVersion: 1,
				Plugin:        ManifestPlugin{Name: ""},
			},
			want: "plugin.name",
		},
		{
			name: "invalid path",
			m: Manifest{
				SchemaVersion: 1,
				Plugin:        ManifestPlugin{Name: "ok"},
				Skills: map[string]ManifestEntry{
					"bad": {Path: "../escape"},
				},
			},
			want: "invalid",
		},
		{
			name: "path with dot segment before clean",
			m: Manifest{
				SchemaVersion: 1,
				Plugin:        ManifestPlugin{Name: "ok"},
				Skills: map[string]ManifestEntry{
					"bad": {Path: "skills/../x"},
				},
			},
			want: "invalid",
		},
		{
			name: "nested array metadata unsupported",
			m: Manifest{
				SchemaVersion: 1,
				Plugin: ManifestPlugin{
					Name: "ok",
					Metadata: map[string]any{
						"tags": []any{[]any{"nested"}},
					},
				},
			},
			want: "non-scalar",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateManifest(tt.m)
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("ValidateManifest() error = %v, want ErrInvalidManifest", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateManifest() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
