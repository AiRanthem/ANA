package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing/fstest"
)

const (
	manifestPathDefault     = "manifest.toml"
	defaultMaxExpandedBytes = int64(256 << 20) // 256 MiB
)

type zipReader struct {
	manifest Manifest
	filesys  fs.FS
}

func (r *zipReader) Manifest() Manifest { return cloneManifest(r.manifest) }
func (r *zipReader) FS() fs.FS          { return r.filesys }
func (r *zipReader) Close() error       { return nil }

// OpenZipReader validates and opens an archive backed by io.ReaderAt.
func OpenZipReader(r io.ReaderAt, size int64) (Reader, error) {
	if size < 0 {
		return nil, fmt.Errorf("%w: negative zip size", ErrCorruptArchive)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: open zip: %v", ErrCorruptArchive, err)
	}
	return openFromZip(zr, defaultMaxExpandedBytes)
}

// OpenZipReaderFromStream buffers a zip stream up to maxSize then validates it.
func OpenZipReaderFromStream(ctx context.Context, r io.Reader, sizeHint int64, maxSize int64) (Reader, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("%w: maxSize must be > 0", ErrCorruptArchive)
	}

	var buf bytes.Buffer
	if sizeHint > 0 && sizeHint <= maxSize {
		buf.Grow(int(sizeHint))
	}

	limited := &io.LimitedReader{R: r, N: maxSize + 1}
	written, err := copyWithContext(ctx, &buf, limited)
	if err != nil {
		return nil, err
	}
	if written > maxSize {
		return nil, fmt.Errorf("%w: compressed payload exceeds %d bytes", ErrCorruptArchive, maxSize)
	}
	return OpenZipReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
}

func openFromZip(zr *zip.Reader, maxExpanded int64) (Reader, error) {
	var (
		manifestBytes []byte
		manifest      Manifest
		fileSet       = make(map[string]struct{}, len(zr.File))
		filesys       = make(fstest.MapFS, len(zr.File))
		totalExpanded int64
	)

	for _, f := range zr.File {
		if err := validateZipFileHeader(f); err != nil {
			return nil, err
		}
		fileSet[f.Name] = struct{}{}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %v", ErrCorruptArchive, f.Name, err)
		}
		budget := maxExpanded - totalExpanded
		if budget <= 0 {
			_ = rc.Close()
			return nil, fmt.Errorf("%w: expanded archive exceeds %d bytes", ErrCorruptArchive, maxExpanded)
		}
		// Cap bytes read per entry so a pathological member cannot allocate far beyond maxExpanded
		// before the aggregate guard runs.
		lr := io.LimitedReader{R: rc, N: budget + 1}
		body, err := io.ReadAll(&lr)
		closeErr := rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %v", ErrCorruptArchive, f.Name, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("%w: read %q: close: %v", ErrCorruptArchive, f.Name, closeErr)
		}
		if int64(len(body)) > budget {
			return nil, fmt.Errorf("%w: expanded archive exceeds %d bytes", ErrCorruptArchive, maxExpanded)
		}

		totalExpanded += int64(len(body))

		if f.Name == manifestPathDefault {
			manifestBytes = bytes.Clone(body)
		}
		filesys[f.Name] = &fstest.MapFile{
			Data: body,
			Mode: 0o644,
		}
	}

	if manifestBytes == nil {
		return nil, fmt.Errorf("%w: missing %s", ErrInvalidManifest, manifestPathDefault)
	}
	parsed, err := ParseManifest(manifestBytes)
	if err != nil {
		return nil, err
	}
	manifest = parsed

	if err := validateManifestArchivePaths(manifest, fileSet); err != nil {
		return nil, err
	}

	return &zipReader{
		manifest: manifest,
		filesys:  filesys,
	}, nil
}

func validateZipFileHeader(f *zip.File) error {
	if f == nil {
		return fmt.Errorf("%w: nil zip file header", ErrCorruptArchive)
	}
	if !isSafeArchivePath(f.Name) {
		return fmt.Errorf("%w: invalid zip path %q", ErrCorruptArchive, f.Name)
	}

	mode := f.Mode()
	if mode&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: symlink entry %q is not allowed", ErrCorruptArchive, f.Name)
	}
	return nil
}

func validateManifestArchivePaths(m Manifest, fileSet map[string]struct{}) error {
	var errs []error
	check := func(section Section, entries map[string]ManifestEntry) {
		for key, entry := range entries {
			p := path.Clean(entry.Path)
			if _, ok := fileSet[p]; ok {
				continue
			}
			prefix := strings.TrimSuffix(p, "/") + "/"
			found := false
			for candidate := range fileSet {
				if strings.HasPrefix(candidate, prefix) {
					found = true
					break
				}
			}
			if !found {
				errs = append(errs, fmt.Errorf("%s.%s.path %q does not exist in archive", section, key, entry.Path))
			}
		}
	}

	check(SectionSkills, m.Skills)
	check(SectionRules, m.Rules)
	check(SectionHooks, m.Hooks)
	check(SectionSubagents, m.Subagents)
	check(SectionMCPs, m.MCPs)

	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool {
			return errs[i].Error() < errs[j].Error()
		})
		return fmt.Errorf("%w: %w", ErrInvalidManifest, errors.Join(errs...))
	}
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return written, ctx.Err()
			default:
			}
		}

		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}
