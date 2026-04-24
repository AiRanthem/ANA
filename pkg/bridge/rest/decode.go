package rest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

// DefaultHTTPResponseDecoder selects an event decoder based on the response content type.
func DefaultHTTPResponseDecoder(resp *http.Response) (agentio.EventStream, error) {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))

	switch {
	case strings.Contains(contentType, "text/event-stream"):
		return NewSSEStream(resp.Body), nil
	case strings.Contains(contentType, "application/x-ndjson"),
		strings.Contains(contentType, "application/jsonl"),
		strings.Contains(contentType, "application/json-seq"):
		return NewJSONLStream(resp.Body), nil
	default:
		return NewSingleJSONResponseStream(resp.Body), nil
	}
}

// NewSSEStream converts an SSE response body into a canonical event stream.
func NewSSEStream(body io.ReadCloser) agentio.EventStream {
	ch := make(chan agentio.Event, 128)
	go func() {
		defer close(ch)
		defer body.Close()

		parseSSE(body, ch)
		ch <- agentio.Event{Type: agentio.EventDone, At: time.Now()}
	}()
	return agentio.NewChannelStream(ch, body.Close)
}

// NewJSONLStream converts a JSONL response body into a canonical event stream.
func NewJSONLStream(body io.ReadCloser) agentio.EventStream {
	ch := make(chan agentio.Event, 128)
	go func() {
		defer close(ch)
		defer body.Close()

		parseJSONL(body, ch)
		ch <- agentio.Event{Type: agentio.EventDone, At: time.Now()}
	}()
	return agentio.NewChannelStream(ch, body.Close)
}

// NewSingleJSONResponseStream converts a single JSON response body into events.
func NewSingleJSONResponseStream(body io.ReadCloser) agentio.EventStream {
	ch := make(chan agentio.Event, 4)
	go func() {
		defer close(ch)
		defer body.Close()

		parseSingleJSON(body, ch)
		ch <- agentio.Event{Type: agentio.EventDone, At: time.Now()}
	}()
	return agentio.NewChannelStream(ch, body.Close)
}

func parseSSE(r io.Reader, ch chan<- agentio.Event) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var eventName string
	var dataLines []string

	flush := func() {
		if len(dataLines) == 0 && eventName == "" {
			return
		}

		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		switch data {
		case "":
		case "[DONE]":
			ch <- agentio.Event{Type: agentio.EventDone, At: time.Now()}
		default:
			events, err := decodeFramedPayload(eventName, []byte(data))
			if err != nil {
				ch <- agentio.Event{
					Type: agentio.EventFailure,
					Err: &agentio.EventError{
						Code:    "sse_decode_error",
						Message: err.Error(),
					},
					At: time.Now(),
				}
			} else {
				for _, event := range events {
					ch <- event
				}
			}
		}

		eventName = ""
		dataLines = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}

		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		ch <- agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    "sse_scan_error",
				Message: err.Error(),
			},
			At: time.Now(),
		}
	}
}

func parseJSONL(r io.Reader, ch chan<- agentio.Event) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		events, err := decodeFramedPayload("", []byte(line))
		if err != nil {
			ch <- agentio.Event{
				Type: agentio.EventFailure,
				Err: &agentio.EventError{
					Code:    "jsonl_decode_error",
					Message: err.Error(),
				},
				At: time.Now(),
			}
			continue
		}
		for _, event := range events {
			ch <- event
		}
	}

	if err := scanner.Err(); err != nil {
		ch <- agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    "jsonl_scan_error",
				Message: err.Error(),
			},
			At: time.Now(),
		}
	}
}

func parseSingleJSON(r io.Reader, ch chan<- agentio.Event) {
	body, err := io.ReadAll(r)
	if err != nil {
		ch <- agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    "json_read_error",
				Message: err.Error(),
			},
			At: time.Now(),
		}
		return
	}

	events, err := decodeFramedPayload("", body)
	if err != nil {
		ch <- agentio.Event{
			Type: agentio.EventFailure,
			Err: &agentio.EventError{
				Code:    "json_decode_error",
				Message: err.Error(),
			},
			At: time.Now(),
		}
		return
	}

	for _, event := range events {
		ch <- event
	}
}

func decodeFramedPayload(name string, payload []byte) ([]agentio.Event, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return []agentio.Event{{Type: agentio.EventDone, At: time.Now()}}, nil
	}

	events, err := defaultFrameDecoder(payload)
	if err != nil {
		return nil, err
	}
	if name != "" {
		for i := range events {
			if events[i].Name == "" {
				events[i].Name = name
			}
		}
	}
	return events, nil
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
