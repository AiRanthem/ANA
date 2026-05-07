package plugin

func clonePlugin(p Plugin) Plugin {
	p.Manifest = cloneManifest(p.Manifest)
	return p
}

func cloneManifest(m Manifest) Manifest {
	m.Plugin.Metadata = cloneMapAny(m.Plugin.Metadata)
	m.Skills = cloneManifestEntries(m.Skills)
	m.Rules = cloneManifestEntries(m.Rules)
	m.Hooks = cloneManifestEntries(m.Hooks)
	m.Subagents = cloneManifestEntries(m.Subagents)
	m.MCPs = cloneManifestEntries(m.MCPs)
	return m
}

func cloneManifestEntries(in map[string]ManifestEntry) map[string]ManifestEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ManifestEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneMapAny(in map[string]any) map[string]any {
	return cloneMapAnyDepth(in, 0)
}

func cloneMapAnyDepth(in map[string]any, depth int) map[string]any {
	if depth > maxMetadataNestingDepth {
		return nil
	}
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCloneAnyDepth(v, depth+1)
	}
	return out
}

func deepCloneAnyDepth(v any, depth int) any {
	if depth > maxMetadataNestingDepth {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		return cloneMapAnyDepth(x, depth)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCloneAnyDepth(x[i], depth+1)
		}
		return out
	default:
		return v
	}
}
