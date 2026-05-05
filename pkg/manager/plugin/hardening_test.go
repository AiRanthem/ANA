package plugin

import (
	"errors"
	"testing"
)

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
