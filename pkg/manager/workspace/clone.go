package workspace

import "slices"

func cloneWorkspace(w Workspace) Workspace {
	w.InfraOptions = cloneOptions(w.InfraOptions)
	w.InstallParams = cloneMapAny(w.InstallParams)
	w.Plugins = cloneAttachedPlugins(w.Plugins)
	w.StatusError = cloneError(w.StatusError)
	w.Labels = cloneLabels(w.Labels)
	return w
}

func cloneOptions(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCloneAny(v)
	}
	return out
}

func cloneMapAny(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCloneAny(v)
	}
	return out
}

func cloneAttachedPlugins(in []AttachedPlugin) []AttachedPlugin {
	if len(in) == 0 {
		return nil
	}
	out := make([]AttachedPlugin, len(in))
	copy(out, in)
	for i := range out {
		out[i].PlacedPaths = slices.Clone(out[i].PlacedPaths)
	}
	return out
}

func cloneError(in *Error) *Error {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func deepCloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMapAny(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCloneAny(x[i])
		}
		return out
	default:
		return v
	}
}
