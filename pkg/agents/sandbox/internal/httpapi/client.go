// Package httpapi contains transport shared by sandbox provider adapters.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox"
)

type Client struct {
	Base    string
	HTTP    *http.Client
	Headers http.Header
}

func New(base string, client *http.Client, headers http.Header) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("invalid sandbox API endpoint")
	}
	if client == nil {
		client = &http.Client{}
	}
	return &Client{strings.TrimRight(base, "/"), client, headers.Clone()}, nil
}
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header = c.Headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == 404 {
			return nil, fmt.Errorf("%w: %s", sandbox.ErrNotFound, b)
		}
		if resp.StatusCode == http.StatusConflict {
			return nil, fmt.Errorf("%w: %s", sandbox.ErrConflict, b)
		}
		return nil, fmt.Errorf("sandbox API HTTP %d: %s", resp.StatusCode, b)
	}
	return resp, nil
}
func (c *Client) JSON(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.Do(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output)
}
