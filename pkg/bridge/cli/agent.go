package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

// Agent wraps a subprocess transport and exposes it as a canonical agent.
type Agent struct {
	NameStr string
	Command string
	Args    []string
	Dir     string
	Env     []string

	// json (default) or text
	StdinFormat string
	// stream-json, json, or text
	StdoutFormat string

	EncodeStdin func(*agentio.InvokeRequest) ([]byte, error)
}

// CLIAgent preserves the old transport-specific type name within the subpackage.
type CLIAgent = Agent

func (a *Agent) Name() string {
	if a.NameStr == "" {
		return "cli-agent"
	}
	return a.NameStr
}

func (a *Agent) Invoke(ctx context.Context, req *agentio.InvokeRequest) (agentio.EventStream, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}
	if strings.TrimSpace(a.Command) == "" {
		return nil, errors.New("cli command is empty")
	}

	cmd := exec.CommandContext(ctx, a.Command, a.Args...)
	cmd.Dir = a.Dir
	if len(a.Env) > 0 {
		cmd.Env = append(cmd.Env, a.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open stderr pipe: %w", err)
	}

	encode := a.EncodeStdin
	if encode == nil {
		switch strings.ToLower(a.StdinFormat) {
		case "", "json":
			encode = agentio.EncodeCanonicalRequestJSON
		case "text":
			encode = func(req *agentio.InvokeRequest) ([]byte, error) {
				return []byte(flattenText(req.Parts)), nil
			}
		default:
			return nil, fmt.Errorf("unsupported stdin format: %s", a.StdinFormat)
		}
	}

	input, err := encode(req)
	if err != nil {
		return nil, fmt.Errorf("encode stdin payload: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cli process: %w", err)
	}

	ch := make(chan agentio.Event, 128)
	var once sync.Once
	var producers sync.WaitGroup
	stderrDone := make(chan struct{})
	closeStream := func() error {
		once.Do(func() {
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		return nil
	}

	go func() {
		defer stdin.Close()
		_, _ = stdin.Write(input)
	}()

	producers.Add(1)
	go func() {
		defer producers.Done()
		defer close(stderrDone)
		scanTextLines(stderr, "stderr", ch)
	}()

	producers.Add(1)
	go func() {
		defer producers.Done()

		doneSeen := false
		switch strings.ToLower(a.StdoutFormat) {
		case "", "stream-json":
			parseJSONL(stdout, ch, &doneSeen)
		case "json":
			parseSingleJSON(stdout, ch, &doneSeen)
		case "text":
			scanTextLines(stdout, "stdout", ch)
		default:
			ch <- agentio.Event{
				Type: agentio.EventFailure,
				Err: &agentio.EventError{
					Code:    "unsupported_stdout_format",
					Message: fmt.Sprintf("unsupported stdout format: %s", a.StdoutFormat),
				},
				At: time.Now(),
			}
		}

		<-stderrDone
		if err := cmd.Wait(); err != nil {
			ch <- agentio.Event{
				Type: agentio.EventFailure,
				Err: &agentio.EventError{
					Code:    "process_exit_error",
					Message: err.Error(),
				},
				At: time.Now(),
			}
		}
		emitDoneIfNeeded(ch, &doneSeen)
	}()

	go func() {
		producers.Wait()
		close(ch)
	}()

	return agentio.NewChannelStream(ch, closeStream), nil
}

func flattenText(parts []agentio.InputPart) string {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case agentio.TextPart:
			writeJoinedLine(&b, p.Text)
		case *agentio.TextPart:
			if p != nil {
				writeJoinedLine(&b, p.Text)
			}
		case agentio.JSONPart:
			writeJoinedLine(&b, string(p.Data))
		case *agentio.JSONPart:
			if p != nil {
				writeJoinedLine(&b, string(p.Data))
			}
		case agentio.ToolResultPart:
			writeJoinedLine(&b, "tool_result:"+p.ToolName+"\n"+string(p.Data))
		case *agentio.ToolResultPart:
			if p != nil {
				writeJoinedLine(&b, "tool_result:"+p.ToolName+"\n"+string(p.Data))
			}
		case agentio.BlobPart:
			writeJoinedLine(&b, flattenBlobPart(p))
		case *agentio.BlobPart:
			if p != nil {
				writeJoinedLine(&b, flattenBlobPart(*p))
			}
		}
	}
	return b.String()
}

func writeJoinedLine(b *strings.Builder, line string) {
	if line == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(line)
}

func flattenBlobPart(part agentio.BlobPart) string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(part.Kind)
	b.WriteString(" ")
	switch part.SourceType {
	case agentio.BlobInline:
		b.WriteString("inline")
	case agentio.BlobFile:
		b.WriteString(part.Path)
	case agentio.BlobURL:
		b.WriteString(part.URL)
	default:
		b.WriteString(string(part.SourceType))
	}
	b.WriteString("]")
	return b.String()
}

func scanTextLines(r io.Reader, streamName string, ch chan<- agentio.Event) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			continue
		}
		ch <- agentio.Event{
			Type: agentio.EventTextDelta,
			Text: text + "\n",
			Name: streamName,
			At:   time.Now(),
		}
	}

	if err := scanner.Err(); err != nil {
		ch <- agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    streamName + "_scan_error",
				Message: err.Error(),
			},
			At: time.Now(),
		}
	}
}

func parseJSONL(r io.Reader, ch chan<- agentio.Event, doneSeen *bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		events, err := decodeFramedPayload([]byte(line))
		if err != nil {
			emitEvent(ch, doneSeen, agentio.Event{
				Type: agentio.EventFailure,
				Err: &agentio.EventError{
					Code:    "jsonl_decode_error",
					Message: err.Error(),
				},
				At: time.Now(),
			})
			continue
		}
		for _, event := range events {
			emitEvent(ch, doneSeen, event)
		}
	}

	if err := scanner.Err(); err != nil {
		emitEvent(ch, doneSeen, agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    "jsonl_scan_error",
				Message: err.Error(),
			},
			At: time.Now(),
		})
	}
}

func parseSingleJSON(r io.Reader, ch chan<- agentio.Event, doneSeen *bool) {
	body, err := io.ReadAll(r)
	if err != nil {
		emitEvent(ch, doneSeen, agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    "json_read_error",
				Message: err.Error(),
			},
			At: time.Now(),
		})
		return
	}

	events, err := decodeFramedPayload(body)
	if err != nil {
		emitEvent(ch, doneSeen, agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    "json_decode_error",
				Message: err.Error(),
			},
			At: time.Now(),
		})
		return
	}

	for _, event := range events {
		emitEvent(ch, doneSeen, event)
	}
}

func emitDoneIfNeeded(ch chan<- agentio.Event, doneSeen *bool) {
	if doneSeen != nil {
		*doneSeen = true
	}
	ch <- agentio.Event{Type: agentio.EventDone, At: time.Now()}
}

func emitEvent(ch chan<- agentio.Event, doneSeen *bool, event agentio.Event) {
	if event.Type == agentio.EventDone {
		if doneSeen != nil {
			*doneSeen = true
		}
		return
	}
	ch <- event
}

func decodeFramedPayload(payload []byte) ([]agentio.Event, error) {
	payload = bytesTrimSpace(payload)
	if len(payload) == 0 {
		return nil, nil
	}
	if string(payload) == "[DONE]" {
		return []agentio.Event{{Type: agentio.EventDone, At: time.Now()}}, nil
	}
	return defaultFrameDecoder(payload)
}

func defaultFrameDecoder(payload []byte) ([]agentio.Event, error) {
	var arr []map[string]any
	if err := json.Unmarshal(payload, &arr); err == nil {
		out := make([]agentio.Event, 0, len(arr))
		for _, item := range arr {
			out = append(out, bestEffortEventFromMap(item))
		}
		return out, nil
	}

	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		text := strings.TrimSpace(string(payload))
		if text == "" {
			return nil, nil
		}
		return []agentio.Event{{Type: agentio.EventTextDelta, Text: text, At: time.Now()}}, nil
	}

	return []agentio.Event{bestEffortEventFromMap(obj)}, nil
}

func bestEffortEventFromMap(m map[string]any) agentio.Event {
	event := agentio.Event{
		Type:     agentio.EventStructured,
		Metadata: map[string]any{},
		At:       time.Now(),
	}

	if eventType, ok := stringField(m, "type"); ok {
		event.Type = agentio.EventType(eventType)
	}
	if text, ok := firstStringField(m, "text", "delta", "output_text", "message"); ok {
		event.Text = text
		if event.Type == agentio.EventStructured {
			event.Type = agentio.EventTextDelta
		}
	}
	if name, ok := firstStringField(m, "name", "event"); ok {
		event.Name = name
	}
	if usage, ok := m["usage"]; ok {
		if raw, err := json.Marshal(usage); err == nil {
			var decoded agentio.Usage
			if json.Unmarshal(raw, &decoded) == nil {
				event.Usage = &decoded
				if event.Type == agentio.EventStructured {
					event.Type = agentio.EventUsage
				}
			}
		}
	}
	if rawErr, ok := m["error"]; ok {
		switch typed := rawErr.(type) {
		case string:
			event.Err = &agentio.EventError{Message: typed}
		default:
			if raw, err := json.Marshal(typed); err == nil {
				var decoded agentio.EventError
				if json.Unmarshal(raw, &decoded) == nil && decoded.Message != "" {
					event.Err = &decoded
				} else {
					event.Err = &agentio.EventError{Message: string(raw)}
				}
			}
		}
		if event.Err != nil {
			event.Type = agentio.EventFailure
		}
	}
	if event.Text == "" {
		if raw, err := json.Marshal(m); err == nil {
			event.JSON = raw
		}
	}

	return event
}

func bytesTrimSpace(payload []byte) []byte {
	return []byte(strings.TrimSpace(string(payload)))
}

func stringField(m map[string]any, key string) (string, bool) {
	value, ok := m[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func firstStringField(m map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if text, ok := stringField(m, key); ok && text != "" {
			return text, true
		}
	}
	return "", false
}
