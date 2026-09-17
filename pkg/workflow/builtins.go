package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewJavaScriptNode constructs a synchronous JavaScript node.
func NewJavaScriptNode(id string, cfg JavaScriptNodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "javascript")
	if err != nil {
		return nil, err
	}
	timeout, err := nodeTimeout(cfg.Timeout)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(cfg.Code) == "" {
		return nil, fmt.Errorf("code is required")
	}
	code := "(function(){\n" + cfg.Code + "\n})()"
	if err := validateJS(code); err != nil {
		return nil, err
	}
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		v, err := evalJS(ctx, code, in, timeout)
		return v, DefaultPort, err
	}
	return n, nil
}

// NewIfElseNode constructs a boolean branch with true and false ports.
func NewIfElseNode(id string, cfg IfElseNodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "if_else")
	if err != nil {
		return nil, err
	}
	timeout, err := nodeTimeout(cfg.Timeout)
	if err != nil {
		return nil, err
	}

	if cfg.Condition == "" {
		return nil, fmt.Errorf("condition is required")
	}
	if err := validateJS(expression(cfg.Condition)); err != nil {
		return nil, err
	}
	n.ports = map[string]bool{"true": true, "false": true}
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		v, err := evalJS(ctx, expression(cfg.Condition), in, timeout)
		if err != nil {
			return nil, "", err
		}
		b, ok := v.(bool)
		if !ok {
			return nil, "", fmt.Errorf("condition must return a boolean")
		}
		port := "false"
		if b {
			port = "true"
		}
		return b, port, nil
	}
	return n, nil
}

// NewSwitchNode constructs a switch node with the same behavior as YAML.
func NewSwitchNode(id string, cfg SwitchNodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "switch")
	if err != nil {
		return nil, err
	}
	timeout, err := nodeTimeout(cfg.Timeout)
	if err != nil {
		return nil, err
	}
	cfg.Value, err = copyBindings(cfg.Value)
	if err != nil {
		return nil, err
	}
	var cases []SwitchCase
	if err := decodeValue(cfg.Cases, &cases); err != nil {
		return nil, err
	}
	cfg.Cases = cases
	resolve := func(ctx context.Context, v any, in *Input) (any, error) { return resolveValue(ctx, v, in, timeout) }

	if len(cfg.Cases) == 0 {
		return nil, fmt.Errorf("cases are required")
	}
	seen := map[string]bool{}
	for _, c := range cfg.Cases {
		if c.Port == "" || c.Port == DefaultPort {
			return nil, fmt.Errorf("case port must be non-empty and not default")
		}
		b, err := json.Marshal(c.Value)
		if err != nil {
			return nil, err
		}
		if seen[string(b)] {
			return nil, fmt.Errorf("duplicate switch case")
		}
		seen[string(b)] = true
		n.ports[c.Port] = true
	}
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		v, err := resolve(ctx, cfg.Value, in)
		if err != nil {
			return nil, "", err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, "", err
		}
		for _, c := range cfg.Cases {
			cb, _ := json.Marshal(c.Value)
			if bytes.Equal(b, cb) {
				return v, c.Port, nil
			}
		}
		return v, DefaultPort, nil
	}
	return n, nil
}

// NewDelayNode constructs a delay node with the same behavior as YAML.
func NewDelayNode(id string, cfg DelayNodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "delay")
	if err != nil {
		return nil, err
	}

	duration := cfg.Duration
	if duration < 0 {
		return nil, fmt.Errorf("duration must not be negative")
	}
	n.delay = &duration
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-timer.C:
			return map[string]any{"duration": cfg.Duration.String()}, DefaultPort, nil
		}
	}
	return n, nil
}

// NewAPINode constructs an HTTP API call node.
func NewAPINode(id string, cfg APINodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "api")
	if err != nil {
		return nil, err
	}
	expressionTimeout, err := nodeTimeout(cfg.ExpressionTimeout)
	if err != nil {
		return nil, err
	}
	cfg.Body, err = copyBindings(cfg.Body)
	if err != nil {
		return nil, err
	}
	var headers map[string]string
	if err := decodeValue(cfg.Headers, &headers); err != nil {
		return nil, err
	}
	cfg.Headers = headers
	if _, err := copyBindings(map[string]any{"url": cfg.URL, "headers": cfg.Headers}); err != nil {
		return nil, err
	}
	if cfg.MaxResponseBytes < 0 || cfg.MaxResponseBytes == math.MaxInt64 {
		return nil, fmt.Errorf("invalid maximum response size")
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 4 << 20
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	resolve := func(ctx context.Context, v any, in *Input) (any, error) {
		return resolveValue(ctx, v, in, expressionTimeout)
	}

	if cfg.URL == "" {
		return nil, fmt.Errorf("url is required")
	}
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}
	cfg.Method = strings.ToUpper(cfg.Method)
	if _, err := http.NewRequest(cfg.Method, "http://localhost", nil); err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout < 0 {
		return nil, fmt.Errorf("invalid API timeout")
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		address, err := resolve(ctx, cfg.URL, in)
		if err != nil {
			return nil, "", err
		}
		addressString, ok := address.(string)
		if !ok {
			return nil, "", fmt.Errorf("url must resolve to a string")
		}
		u, err := url.Parse(addressString)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, "", fmt.Errorf("API url must be absolute HTTP(S)")
		}
		var body io.Reader
		if cfg.Body != nil {
			v, err := resolve(ctx, cfg.Body, in)
			if err != nil {
				return nil, "", err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, "", err
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, cfg.Method, addressString, body)
		if err != nil {
			return nil, "", err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range cfg.Headers {
			value, err := resolve(ctx, v, in)
			if err != nil {
				return nil, "", err
			}
			text, ok := value.(string)
			if !ok {
				return nil, "", fmt.Errorf("header %s must resolve to a string", k)
			}
			req.Header.Set(k, text)
		}
		response, err := cfg.HTTPClient.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, cfg.MaxResponseBytes+1))
		if err != nil {
			return nil, "", err
		}
		if int64(len(data)) > cfg.MaxResponseBytes {
			return nil, "", fmt.Errorf("API response exceeds %d bytes", cfg.MaxResponseBytes)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, "", fmt.Errorf("API returned HTTP %d", response.StatusCode)
		}
		var value any = string(data)
		if json.Valid(data) {
			if err := json.Unmarshal(data, &value); err != nil {
				return nil, "", err
			}
		}
		return map[string]any{"status": response.StatusCode, "headers": response.Header, "body": value}, DefaultPort, nil
	}
	return n, nil
}

// NewHumanNode constructs an approval or form node with approved and rejected ports.
func NewHumanNode(id string, cfg HumanNodeConfig) (Node, error) {
	n, err := newBuiltinNode(id, "human")
	if err != nil {
		return nil, err
	}
	timeout, err := nodeTimeout(cfg.Timeout)
	if err != nil {
		return nil, err
	}
	if _, err := copyBindings(cfg.Message); err != nil {
		return nil, err
	}
	var schemaCopy map[string]any
	if err := decodeValue(cfg.Schema, &schemaCopy); err != nil {
		return nil, err
	}
	cfg.Schema = schemaCopy
	resolve := func(ctx context.Context, v any, in *Input) (any, error) { return resolveValue(ctx, v, in, timeout) }

	if cfg.Message == "" {
		return nil, fmt.Errorf("message is required")
	}
	var schema *jsonschema.Resolved
	if cfg.Schema != nil {
		var raw jsonschema.Schema
		if err := decodeValue(cfg.Schema, &raw); err != nil {
			return nil, fmt.Errorf("human schema: %w", err)
		}
		var err error
		schema, err = raw.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("human schema: %w", err)
		}
	}
	n.ports = map[string]bool{"approved": true, "rejected": true}
	n.execute = func(ctx context.Context, in *Input) (any, string, error) {
		if decision, ok := in.Resume(id); ok {
			action, _ := decision["action"].(string)
			switch action {
			case responses.InterruptActionApprove:
				if schema != nil {
					if err := schema.Validate(decision["content"]); err != nil {
						return nil, "", fmt.Errorf("human response: %w", err)
					}
				}
				return decision, "approved", nil
			case responses.InterruptActionReject:
				return decision, "rejected", nil
			default:
				return nil, "", fmt.Errorf("human decision requires action approve or reject")
			}
		}
		message, err := resolve(ctx, cfg.Message, in)
		if err != nil {
			return nil, "", err
		}
		text, ok := message.(string)
		if !ok {
			return nil, "", fmt.Errorf("human message must be a string")
		}
		intr := responses.Interrupt{FunctionCallMessage: nodeCall(in, id, "human", `{}`), Mode: responses.InterruptModeApproval}
		// Arguments make the prompt visible for clients rendering plain approvals.
		args, _ := json.Marshal(map[string]any{"message": text})
		intr.FunctionCallMessage.Arguments = string(args)
		if cfg.Schema != nil {
			intr.Mode = responses.InterruptModeForm
			intr.Elicitations = []mcp.ElicitParams{{Mode: "form", Message: text, RequestedSchema: cfg.Schema}}
		}
		return nil, "", Pause(map[string]any{"interrupts": []responses.Interrupt{intr}})
	}
	return n, nil
}
