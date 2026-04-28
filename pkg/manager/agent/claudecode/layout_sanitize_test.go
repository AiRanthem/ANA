package claudecode

import (
	"strings"
	"testing"
)

func TestSanitizePluginName_TruncatedNamesRemainUnique(t *testing.T) {
	t.Parallel()

	base := strings.Repeat("verylongpluginname-", 8)
	a := sanitizePluginName(base + "variant-a")
	b := sanitizePluginName(base + "variant-b")

	if a == b {
		t.Fatalf("sanitizePluginName returned collision: %q", a)
	}
	if len(a) > 64 || len(b) > 64 {
		t.Fatalf("sanitized names exceed limit: len(a)=%d len(b)=%d", len(a), len(b))
	}
}
