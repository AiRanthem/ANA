package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/AiRanthem/ANA/pkg/manager/agent"
	"github.com/AiRanthem/ANA/pkg/manager/infraops"
	"github.com/AiRanthem/ANA/pkg/manager/plugin"
)

const (
	manifestFile = "manifest.toml"
)

var canonicalSectionRoots = []string{
	"skills",
	"rules",
	"hooks",
	"subagents",
	"mcps",
	"assets",
}

type layout struct{}

func newLayout() *layout {
	return &layout{}
}

type writePlan struct {
	source string
	target string
	mode   fs.FileMode
}

func (l *layout) PluginDirectoryKey(manifest plugin.Manifest) (string, error) {
	pluginName := sanitizePluginName(manifest.Plugin.Name)
	if pluginName == "" {
		return "", fmt.Errorf("%w: plugin name %q sanitizes to empty", agent.ErrInvalidPluginLayout, manifest.Plugin.Name)
	}
	return path.Join(".claude", "plugins", pluginName), nil
}

func (l *layout) Apply(ctx context.Context, ops infraops.InfraOps, manifest plugin.Manifest, pluginRoot fs.FS) ([]string, error) {
	pluginBase, err := l.PluginDirectoryKey(manifest)
	if err != nil {
		return nil, err
	}
	if err := ensureNoPluginNameCollision(ctx, ops, pluginBase); err != nil {
		return nil, err
	}

	plans, err := buildWritePlan(pluginRoot, pluginBase)
	if err != nil {
		return nil, err
	}

	placed := make([]string, 0, len(plans))
	for _, plan := range plans {
		content, err := pluginRoot.Open(plan.source)
		if err != nil {
			return nil, fmt.Errorf("claudecode layout open %q: %w", plan.source, err)
		}

		putErr := ops.PutFile(ctx, plan.target, content, plan.mode)
		closeErr := content.Close()
		if putErr != nil {
			return nil, fmt.Errorf("claudecode layout put %q: %w", plan.target, putErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("claudecode layout close %q: %w", plan.source, closeErr)
		}

		placed = append(placed, plan.target)
	}

	return placed, nil
}

// LayoutPaths returns the deterministic Claude Code path plan for manifest-defined paths.
func LayoutPaths(manifest plugin.Manifest) []string {
	pluginName := sanitizePluginName(manifest.Plugin.Name)
	if pluginName == "" {
		return nil
	}
	pluginBase := path.Join(".claude", "plugins", pluginName)

	pathCap := 1 + len(manifest.Skills) + len(manifest.Rules) + len(manifest.Hooks) +
		len(manifest.Subagents) + len(manifest.MCPs)
	seen := make(map[string]struct{}, pathCap)
	paths := make([]string, 0, pathCap)

	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	add(path.Join(pluginBase, manifestFile))
	addManifestSectionPaths(add, pluginBase, manifest.Skills)
	addManifestSectionPaths(add, pluginBase, manifest.Rules)
	addManifestSectionPaths(add, pluginBase, manifest.Hooks)
	addManifestSectionPaths(add, pluginBase, manifest.Subagents)
	addManifestSectionPaths(add, pluginBase, manifest.MCPs)

	sort.Strings(paths)
	return paths
}

func addManifestSectionPaths(add func(string), pluginBase string, entries map[string]plugin.ManifestEntry) {
	if len(entries) == 0 {
		return
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := entries[key]
		cleanPath := path.Clean(strings.TrimSpace(entry.Path))
		if layoutPathUnsafe(cleanPath) {
			continue
		}
		add(path.Join(pluginBase, cleanPath))
	}
}

func buildWritePlan(pluginRoot fs.FS, pluginBase string) ([]writePlan, error) {
	planned := make([]writePlan, 0, 32)
	targets := make(map[string]string, 32)

	err := fs.WalkDir(pluginRoot, ".", func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if srcPath == "." || d.IsDir() {
			return nil
		}

		targetPath, err := mapPath(pluginBase, srcPath)
		if err != nil {
			return err
		}
		if previous, exists := targets[targetPath]; exists {
			return fmt.Errorf("%w: target path collision %q from %q and %q", ErrInvalidPluginLayout, targetPath, previous, srcPath)
		}
		targets[targetPath] = srcPath

		mode, err := fileMode(d)
		if err != nil {
			return fmt.Errorf("stat %q: %w", srcPath, err)
		}

		planned = append(planned, writePlan{
			source: srcPath,
			target: targetPath,
			mode:   mode,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claudecode layout walk plugin fs: %w", err)
	}

	sort.SliceStable(planned, func(i, j int) bool {
		if planned[i].source == manifestFile && planned[j].source != manifestFile {
			return true
		}
		if planned[j].source == manifestFile && planned[i].source != manifestFile {
			return false
		}
		return planned[i].source < planned[j].source
	})
	return planned, nil
}

func layoutPathUnsafe(cleanPath string) bool {
	if cleanPath == "." || cleanPath == "/" || path.IsAbs(cleanPath) {
		return true
	}
	if strings.HasPrefix(cleanPath, "../") {
		return true
	}
	for _, seg := range strings.Split(cleanPath, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func mapPath(pluginBase, srcPath string) (string, error) {
	clean := path.Clean(srcPath)
	if layoutPathUnsafe(clean) {
		return "", fmt.Errorf("%w: unsafe path %q", ErrInvalidPluginLayout, srcPath)
	}

	switch clean {
	case manifestFile, "AGENTS.md", "README.md":
		return path.Join(pluginBase, clean), nil
	}

	for _, section := range canonicalSectionRoots {
		if clean == section || strings.HasPrefix(clean, section+"/") {
			return path.Join(pluginBase, clean), nil
		}
	}

	// Only root-level extra files are allowed outside canonical sections.
	if strings.Contains(clean, "/") {
		return "", fmt.Errorf("%w: non-canonical nested path %q", ErrInvalidPluginLayout, srcPath)
	}
	return path.Join(pluginBase, clean), nil
}

func fileMode(d fs.DirEntry) (fs.FileMode, error) {
	info, err := d.Info()
	if err != nil {
		return 0, err
	}

	mode := fs.FileMode(0o644)
	if info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	return mode, nil
}

func ensureNoPluginNameCollision(ctx context.Context, ops infraops.InfraOps, pluginBase string) error {
	occupied, err := pathOccupied(ctx, ops, pluginBase)
	if err != nil {
		return fmt.Errorf("claudecode layout check plugin base %q: %w", pluginBase, err)
	}
	if occupied {
		return fmt.Errorf("%w: plugin directory collision %q", ErrInvalidPluginLayout, pluginBase)
	}

	manifestPath := path.Join(pluginBase, manifestFile)
	occupied, err = pathOccupied(ctx, ops, manifestPath)
	if err != nil {
		return fmt.Errorf("claudecode layout check existing plugin %q: %w", pluginBase, err)
	}
	if occupied {
		return fmt.Errorf("%w: plugin directory collision %q", ErrInvalidPluginLayout, pluginBase)
	}

	return nil
}

func pathOccupied(ctx context.Context, ops infraops.InfraOps, p string) (bool, error) {
	rc, err := ops.GetFile(ctx, p)
	if err == nil {
		_ = rc.Close()
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, infraops.ErrNotARegularFile) {
		return true, nil
	}
	return false, err
}

func sanitizePluginName(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(normalized))

	prevDash := false
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-':
			if b.Len() == 0 || prevDash {
				continue
			}
			b.WriteRune(r)
			prevDash = true
		default:
			if b.Len() == 0 || prevDash {
				continue
			}
			b.WriteRune('-')
			prevDash = true
		}
	}

	clean := strings.Trim(b.String(), "-")
	if len(clean) > 64 {
		sum := sha256.Sum256([]byte(clean))
		suffix := hex.EncodeToString(sum[:4])
		const maxPrefix = 64 - 1 - 8 // "-" + 8 hex chars
		prefix := strings.Trim(clean[:maxPrefix], "-")
		if prefix == "" {
			prefix = "plugin"
		}
		clean = prefix + "-" + suffix
	}
	return clean
}
