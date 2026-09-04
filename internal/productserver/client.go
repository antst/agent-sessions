// Package productserver provides the small HTTP surface shared by native
// product adapters. Product routes and payloads stay in the product packages.
package productserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/antst/agent-sessions/internal/productruntime"
)

var (
	ErrNonLoopback          = errors.New("product server endpoint is invalid")
	ErrInvalidLimits        = errors.New("product server limits are invalid")
	ErrInvalidAuth          = errors.New("product server memory auth is invalid")
	ErrInvalidRequestTarget = errors.New("product server request target must be an origin-relative path")
	ErrRequestTooLarge      = errors.New("product server request is too large")
	ErrResponseTooLarge     = errors.New("product server response is too large")
	ErrInvalidResponse      = errors.New("product server response is invalid")
)

const (
	defaultMaxRequestBodyBytes  int64 = 8 << 20
	defaultMaxResponseBodyBytes int64 = 16 << 20
)

// Limits retains the shared adapter configuration shape. The plain client
// uses the body limits; the former wire/header/replay machinery is gone.
type Limits struct {
	MaxRequestBodyBytes  int64
	MaxResponseBodyBytes int64
}

func (limits Limits) normalized() (Limits, error) {
	if limits.MaxRequestBodyBytes < 0 || limits.MaxResponseBodyBytes < 0 {
		return Limits{}, ErrInvalidLimits
	}
	if limits.MaxRequestBodyBytes == 0 {
		limits.MaxRequestBodyBytes = defaultMaxRequestBodyBytes
	}
	if limits.MaxResponseBodyBytes == 0 {
		limits.MaxResponseBodyBytes = defaultMaxResponseBodyBytes
	}
	return limits, nil
}

type MemoryAuth struct {
	header string
	value  productruntime.SensitiveValue
}

func NewMemoryAuth(header string, value productruntime.SensitiveValue) (MemoryAuth, error) {
	if strings.TrimSpace(header) == "" || strings.ContainsAny(header, "\r\n:") || value.Empty() || strings.ContainsAny(value.Reveal(), "\r\n") {
		return MemoryAuth{}, ErrInvalidAuth
	}
	return MemoryAuth{header: http.CanonicalHeaderKey(header), value: value}, nil
}

func NewBearerAuth(secret productruntime.SensitiveValue) (MemoryAuth, error) {
	if secret.Empty() {
		return MemoryAuth{}, ErrInvalidAuth
	}
	return NewMemoryAuth("Authorization", productruntime.NewSensitiveValue("Bearer "+secret.Reveal()))
}

func NewBasicAuth(username string, password productruntime.SensitiveValue) (MemoryAuth, error) {
	if username == "" || strings.ContainsAny(username, ":\r\n") || password.Empty() {
		return MemoryAuth{}, ErrInvalidAuth
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password.Reveal()))
	return NewMemoryAuth("Authorization", productruntime.NewSensitiveValue("Basic "+encoded))
}

func (auth MemoryAuth) valid() bool { return auth.header != "" && !auth.value.Empty() }

func (auth MemoryAuth) Apply(header http.Header) {
	if auth.valid() {
		header.Set(auth.header, auth.value.Reveal())
	}
}

func (auth MemoryAuth) Authorize(request *http.Request) bool {
	return request != nil && auth.valid() && request.Header.Get(auth.header) == auth.value.Reveal()
}

func (MemoryAuth) String() string   { return "[REDACTED]" }
func (MemoryAuth) GoString() string { return "[REDACTED]" }
func (MemoryAuth) MarshalJSON() ([]byte, error) {
	return nil, ErrInvalidAuth
}

type ClientConfig struct {
	Endpoint string
	Auth     MemoryAuth
	Limits   Limits
}

type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type Client struct {
	base       *url.URL
	auth       MemoryAuth
	limits     Limits
	httpClient *http.Client
}

func NewClient(config ClientConfig) (*Client, error) {
	base, err := url.Parse(config.Endpoint)
	if err != nil || base.Scheme != "http" || base.Host == "" || !loopbackHost(base.Hostname()) {
		return nil, ErrNonLoopback
	}
	if !config.Auth.valid() {
		return nil, ErrInvalidAuth
	}
	limits, err := config.Limits.normalized()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil}
	return &Client{
		base: base, auth: config.Auth, limits: limits,
		httpClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrNonLoopback
		}},
	}, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (client *Client) Do(ctx context.Context, request Request) (Response, error) {
	response, err := client.doRaw(ctx, request)
	if err != nil {
		return Response{}, err
	}
	defer response.Body.Close()
	body, err := readBody(response.Body, client.limits.MaxResponseBodyBytes)
	if err != nil {
		return Response{}, err
	}
	return Response{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: body}, nil
}

func (client *Client) doRaw(ctx context.Context, request Request) (*http.Response, error) {
	if client == nil || client.httpClient == nil || ctx == nil || request.Method == "" {
		return nil, ErrInvalidRequestTarget
	}
	if int64(len(request.Body)) > client.limits.MaxRequestBodyBytes {
		return nil, ErrRequestTooLarge
	}
	target, err := client.requestURL(request.Path)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, target.String(), bytes.NewReader(request.Body))
	if err != nil {
		return nil, ErrInvalidRequestTarget
	}
	httpRequest.Header = request.Header.Clone()
	if httpRequest.Header == nil {
		httpRequest.Header = make(http.Header)
	}
	client.auth.Apply(httpRequest.Header)
	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("product server request failed: %w", err)
	}
	return response, nil
}

func (client *Client) requestURL(path string) (*url.URL, error) {
	if path == "" {
		path = "/"
	}
	parsed, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") || parsed.IsAbs() || parsed.Host != "" {
		return nil, ErrInvalidRequestTarget
	}
	target := *client.base
	target.Path, target.RawPath, target.RawQuery = parsed.Path, parsed.RawPath, parsed.RawQuery
	return &target, nil
}

func readBody(reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

func (client *Client) CloseIdleConnections() {
	if client != nil && client.httpClient != nil {
		client.httpClient.CloseIdleConnections()
	}
}

var _ json.Marshaler = MemoryAuth{}
