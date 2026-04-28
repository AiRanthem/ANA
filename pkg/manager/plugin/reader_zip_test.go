package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestOpenZipReader_RejectsEntryExceedingRemainingBudget(t *testing.T) {
	t.Parallel()

	zipBytes := buildZip(t, map[string][]byte{
		"manifest.toml":      []byte(validManifest("p1", "skills/s1")),
		"skills/s1/SKILL.md": bytes.Repeat([]byte("z"), 5000),
	})
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	const maxExpanded = int64(2000)
	_, err = openFromZip(zr, maxExpanded)
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("openFromZip() error = %v, want %v", err, ErrCorruptArchive)
	}
}

func TestOpenZipReader_RejectsInvalidArchives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string][]byte
		want  error
	}{
		{
			name: "missing manifest",
			files: map[string][]byte{
				"README.md": []byte("hi"),
			},
			want: ErrInvalidManifest,
		},
		{
			name: "path traversal entry",
			files: map[string][]byte{
				"manifest.toml": []byte(validManifest("p1", "skills/s1")),
				"../evil.txt":   []byte("x"),
			},
			want: ErrCorruptArchive,
		},
		{
			name: "manifest points to missing path",
			files: map[string][]byte{
				"manifest.toml": []byte(validManifest("p1", "skills/missing")),
			},
			want: ErrInvalidManifest,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			zipBytes := buildZip(t, tt.files)
			_, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
			if !errors.Is(err, tt.want) {
				t.Fatalf("OpenZipReader() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestOpenZipReader_RoundTripFS(t *testing.T) {
	t.Parallel()

	zipBytes := buildZip(t, map[string][]byte{
		"manifest.toml":        []byte(validManifest("demo", "skills/s1")),
		"skills/s1/SKILL.md":   []byte("# skill"),
		"rules/style.mdc":      []byte("rule"),
		"assets/readme.txt":    []byte("asset"),
		"subagents/agent1.txt": []byte("a"),
	})

	r, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("OpenZipReader() error = %v", err)
	}
	defer func() { _ = r.Close() }()

	content, err := fs.ReadFile(r.FS(), "skills/s1/SKILL.md")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "# skill" {
		t.Fatalf("unexpected content: %q", string(content))
	}
	if r.Manifest().Plugin.Name != "demo" {
		t.Fatalf("manifest plugin name mismatch")
	}
}

func TestOpenZipReaderFromStream_MaxSize(t *testing.T) {
	t.Parallel()

	zipBytes := buildZip(t, map[string][]byte{
		"manifest.toml":      []byte(validManifest("demo", "skills/s1")),
		"skills/s1/SKILL.md": bytes.Repeat([]byte("a"), 1024),
	})

	_, err := OpenZipReaderFromStream(context.Background(), bytes.NewReader(zipBytes), int64(len(zipBytes)), 128)
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("OpenZipReaderFromStream() error = %v, want ErrCorruptArchive", err)
	}
}

func TestOpenZipReader_RejectsSymlink(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	addFile := func(name string, data []byte, mode fs.FileMode) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("CreateHeader(%s): %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	addFile("manifest.toml", []byte(validManifest("demo", "skills/s1")), 0o644)
	addFile("skills/s1", []byte("target"), fs.ModeSymlink|0o777)
	if err := zw.Close(); err != nil {
		t.Fatalf("Close zip: %v", err)
	}

	_, err := OpenZipReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("OpenZipReader() error = %v, want ErrCorruptArchive", err)
	}
}

func buildZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close zip: %v", err)
	}
	return buf.Bytes()
}

func validManifest(name string, skillPath string) string {
	return strings.TrimSpace(`
schema_version = 1
[plugin]
name = "`+name+`"

[skills.sample]
path = "`+skillPath+`"
`) + "\n"
}

type zipEntry struct {
	name string
	body []byte
	mode fs.FileMode
}

func buildZipWithDuplicate(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("CreateHeader(%s): %v", e.name, err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatalf("Write(%s): %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close zip: %v", err)
	}
	return buf.Bytes()
}

func buildZipWithMode(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("CreateHeader(%s): %v", e.name, err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatalf("Write(%s): %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close zip: %v", err)
	}
	return buf.Bytes()
}

func TestOpenZipReader_RejectsDuplicateEntries(t *testing.T) {
	t.Parallel()
	zipBytes := buildZipWithDuplicate(t,
		zipEntry{name: "manifest.toml", body: []byte(validManifest("demo", "skills/s1"))},
		zipEntry{name: "manifest.toml", body: []byte(validManifest("demo2", "skills/s1"))},
		zipEntry{name: "skills/s1/SKILL.md", body: []byte("# skill")},
	)
	_, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("OpenZipReader() error = %v, want ErrCorruptArchive", err)
	}
}

func TestOpenZipReader_RejectsDotSegmentPathInArchive(t *testing.T) {
	t.Parallel()
	zipBytes := buildZip(t, map[string][]byte{
		"manifest.toml":        []byte(validManifest("p1", "skills/s1")),
		"skills/./s1/SKILL.md": []byte("# skill"),
		"skills/s1/SKILL.md":   []byte("# other"),
	})
	_, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("OpenZipReader() error = %v, want ErrCorruptArchive", err)
	}
}

func TestOpenZipReader_RejectsDotSegmentPathInManifest(t *testing.T) {
	t.Parallel()
	zipBytes := buildZip(t, map[string][]byte{
		"manifest.toml":      []byte(validManifest("p1", "skills/./s1")),
		"skills/s1/SKILL.md": []byte("# skill"),
	})
	_, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("OpenZipReader() error = %v, want ErrInvalidManifest", err)
	}
}

func TestOpenZipReader_PreservesExecutableBit(t *testing.T) {
	t.Parallel()
	zipBytes := buildZipWithMode(t, []zipEntry{
		{name: "manifest.toml", body: []byte(validManifest("demo", "skills/s1")), mode: 0o644},
		{name: "skills/s1/run.sh", body: []byte("#!/bin/sh\necho hi\n"), mode: 0o755},
	})
	r, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("OpenZipReader() error = %v", err)
	}
	defer func() { _ = r.Close() }()

	info, err := fs.Stat(r.FS(), "skills/s1/run.sh")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("mode = %v, want executable bit", info.Mode())
	}
}

func TestOpenZipReader_AcceptsExplicitDirectoryEntries(t *testing.T) {
	t.Parallel()

	zipBytes := buildZipWithMode(t, []zipEntry{
		{name: "manifest.toml", body: []byte(validManifest("demo", "skills/s1")), mode: 0o644},
		{name: "skills/", body: nil, mode: fs.ModeDir | 0o755},
		{name: "skills/s1/", body: nil, mode: fs.ModeDir | 0o755},
		{name: "skills/s1/SKILL.md", body: []byte("# skill\n"), mode: 0o644},
	})
	r, err := OpenZipReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("OpenZipReader() error = %v", err)
	}
	defer func() { _ = r.Close() }()

	content, err := fs.ReadFile(r.FS(), "skills/s1/SKILL.md")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "# skill\n" {
		t.Fatalf("content = %q", string(content))
	}
	if r.Manifest().Plugin.Name != "demo" {
		t.Fatalf("manifest name = %q, want demo", r.Manifest().Plugin.Name)
	}
}

var _ fs.FS = fstest.MapFS{}
