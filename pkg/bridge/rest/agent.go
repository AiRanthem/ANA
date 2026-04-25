package rest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/AiRanthem/ANA/pkg/agentio"
)

// Agent invokes a one-shot HTTP transport and returns a canonical event stream.
type Agent struct {
	NameStr  string
	Endpoint string
	Method   string
	Headers  map[string]string
	Client   *http.Client

	EncodeRequest  func(*agentio.InvokeRequest) ([]byte, error)
	DecodeResponse func(*http.Response) (agentio.EventStream, error)
}

// RESTAgent preserves the old transport-specific type name within the subpackage.
type RESTAgent = Agent

func (a *Agent) Name() string {
	if a.NameStr == "" {
		return "rest-agent"
	}
	return a.NameStr
}

func (a *Agent) Invoke(ctx context.Context, req *agentio.InvokeRequest) (agentio.EventStream, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}

	method := a.Method
	if method == "" {
		method = http.MethodPost
	}
	if strings.TrimSpace(a.Endpoint) == "" {
		return nil, errors.New("rest agent endpoint is empty")
	}

	encode := a.EncodeRequest
	if encode == nil {
		encode = agentio.EncodeCanonicalRequestJSON
	}
	body, err := encode(req)
	if err != nil {
		return nil, fmt.Errorf("encode rest request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, a.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build rest request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range a.Headers {
		httpReq.Header.Set(k, v)
	}

	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send rest request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("rest agent http %d: %s", resp.StatusCode, string(body))
	}

	decode := a.DecodeResponse
	if decode == nil {
		decode = DefaultHTTPResponseDecoder
	}
	stream, err := decode(resp)
	if err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("decode rest response: %w", err)
	}
	return stream, nil
}
