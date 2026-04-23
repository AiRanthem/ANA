package agentio

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
)

// CanonicalPart is a JSON-safe representation used by the default encoders.
type CanonicalPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Name     string            `json:"name,omitempty"`
	Kind     string            `json:"kind,omitempty"`
	MIMEType string            `json:"mime_type,omitempty"`
	Filename string            `json:"filename,omitempty"`
	Source   map[string]any    `json:"source,omitempty"`
	Data     json.RawMessage   `json:"data,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// CanonicalRequest is the protocol-agnostic JSON envelope used by the default
// REST/CLI/WebSocket encoders. Individual adapters may reshape this.
type CanonicalRequest struct {
	RequestID string            `json:"request_id,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Role      Role              `json:"role,omitempty"`
	Input     []CanonicalPart   `json:"input"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Options   InvokeOptions     `json:"options,omitempty"`
}

// CanonicalizeParts converts parts into a transport-neutral JSON-safe form.
func CanonicalizeParts(parts []InputPart) ([]CanonicalPart, error) {
	out := make([]CanonicalPart, 0, len(parts))
	for i, part := range parts {
		switch p := part.(type) {
		case TextPart:
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("parts[%d]: %w", i, err)
			}
			out = append(out, CanonicalPart{
				Type: "text",
				Text: p.Text,
			})
		case JSONPart:
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("parts[%d]: %w", i, err)
			}
			out = append(out, CanonicalPart{
				Type: "json",
				Name: p.Name,
				Data: p.Data,
			})
		case BlobPart:
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("parts[%d]: %w", i, err)
			}
			source := map[string]any{
				"type": string(p.SourceType),
			}
			switch p.SourceType {
			case BlobInline:
				source["data_base64"] = base64.StdEncoding.EncodeToString(p.Data)
			case BlobFile:
				source["path"] = p.Path
			case BlobURL:
				source["url"] = p.URL
			}
			if p.Size > 0 {
				source["size"] = p.Size
			}
			if p.MIMEType == "" && p.Filename != "" {
				if mt := mime.TypeByExtension(extension(p.Filename)); mt != "" {
					p.MIMEType = mt
				}
			}
			out = append(out, CanonicalPart{
				Type:     p.Kind,
				Kind:     p.Kind,
				MIMEType: p.MIMEType,
				Filename: p.Filename,
				Source:   source,
				Metadata: p.Metadata,
			})
		case ToolResultPart:
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("parts[%d]: %w", i, err)
			}
			out = append(out, CanonicalPart{
				Type: "tool_result",
				Name: p.ToolName,
				Data: p.Data,
				Metadata: map[string]string{
					"call_id": p.CallID,
				},
			})
		default:
			return nil, fmt.Errorf("parts[%d]: unsupported part type %T", i, part)
		}
	}
	return out, nil
}

// ToCanonicalRequest converts InvokeRequest into the default canonical JSON
// envelope.
func ToCanonicalRequest(req *InvokeRequest) (*CanonicalRequest, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	input, err := CanonicalizeParts(req.Parts)
	if err != nil {
		return nil, err
	}
	return &CanonicalRequest{
		RequestID: req.RequestID,
		SessionID: req.SessionID,
		Role:      req.Role,
		Input:     input,
		Metadata:  req.Metadata,
		Options:   req.Options,
	}, nil
}

// EncodeCanonicalRequestJSON is the default encoder for REST/CLI/WebSocket
// adapters.
func EncodeCanonicalRequestJSON(req *InvokeRequest) ([]byte, error) {
	canonical, err := ToCanonicalRequest(req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

func extension(filename string) string {
	if idx := strings.LastIndexByte(filename, '.'); idx >= 0 {
		return filename[idx:]
	}
	return ""
}
