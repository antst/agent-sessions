// Package productserver provides product-neutral safety mechanics for native
// HTTP servers. Product routes, payloads, and lifecycle semantics belong in
// typed product packages above this one.
package productserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/antst/agent-sessions/internal/productruntime"
)

var (
	ErrNonLoopback          = errors.New("product server endpoint must be a literal loopback HTTP address")
	ErrInvalidLimits        = errors.New("product server limits are invalid")
	ErrInvalidAuth          = errors.New("product server memory auth is invalid")
	ErrAuthConflict         = errors.New("request authentication conflicts with product server memory auth")
	ErrInvalidRequestTarget = errors.New("product server request target must be an origin-relative path")
	ErrRequestTooLarge      = errors.New("product server request is too large")
	ErrResponseTooLarge     = errors.New("product server response is too large")
	ErrHeadersTooLarge      = errors.New("product server headers are too large")
	ErrRedirectRefused      = errors.New("product server redirect refused")
	ErrUnsupportedEncoding  = errors.New("product server content encoding is unsupported")
	ErrInvalidResponse      = errors.New("product server response is invalid")
)

const (
	defaultMaxRequestWireBytes  int64 = 1 << 20
	defaultMaxRequestBodyBytes  int64 = 1 << 20
	defaultMaxResponseWireBytes int64 = 8 << 20
	defaultMaxResponseBodyBytes int64 = 8 << 20
	defaultMaxHeaderBytes       int64 = 64 << 10
	defaultDialTimeout                = 5 * time.Second
	defaultHeaderTimeout              = 15 * time.Second
	defaultIdleTimeout                = 30 * time.Second
	defaultMaxConnsPerHost            = 8
	hardMaxRequestBytes         int64 = 64 << 20
	hardMaxResponseBytes        int64 = 64 << 20
	hardMaxHeaderBytes          int64 = 1 << 20
)

// Limits bounds material read or retained by the HTTP and event mechanics.
// A zero field selects a conservative default; negative fields are rejected.
type Limits struct {
	MaxRequestWireBytes  int64
	MaxRequestBodyBytes  int64
	MaxResponseWireBytes int64
	MaxResponseBodyBytes int64
	MaxHeaderBytes       int64
}

func (limits Limits) normalized() (Limits, error) {
	values := []*int64{
		&limits.MaxRequestWireBytes,
		&limits.MaxRequestBodyBytes,
		&limits.MaxResponseWireBytes,
		&limits.MaxResponseBodyBytes,
		&limits.MaxHeaderBytes,
	}
	for _, value := range values {
		if *value < 0 {
			return Limits{}, ErrInvalidLimits
		}
	}
	defaults := []int64{
		defaultMaxRequestWireBytes,
		defaultMaxRequestBodyBytes,
		defaultMaxResponseWireBytes,
		defaultMaxResponseBodyBytes,
		defaultMaxHeaderBytes,
	}
	for index, value := range values {
		if *value == 0 {
			*value = defaults[index]
		}
	}
	if limits.MaxRequestWireBytes > hardMaxRequestBytes || limits.MaxRequestBodyBytes > hardMaxRequestBytes ||
		limits.MaxResponseWireBytes > hardMaxResponseBytes || limits.MaxResponseBodyBytes > hardMaxResponseBytes ||
		limits.MaxHeaderBytes > hardMaxHeaderBytes {
		return Limits{}, ErrInvalidLimits
	}
	return limits, nil
}

// MemoryAuth is an ephemeral exact header credential. Its fields are private,
// formatting is redacted, and JSON serialization always fails.
type MemoryAuth struct {
	header string
	value  productruntime.SensitiveValue
}

// NewMemoryAuth constructs an exact authentication header held only in
// transient memory. The value must include any required scheme.
func NewMemoryAuth(header string, value productruntime.SensitiveValue) (MemoryAuth, error) {
	if !validHeaderName(header) || value.Empty() {
		return MemoryAuth{}, ErrInvalidAuth
	}
	revealed := value.Reveal()
	if strings.ContainsAny(revealed, "\r\n") || int64(len(header)+len(revealed)) > defaultMaxHeaderBytes {
		return MemoryAuth{}, ErrInvalidAuth
	}
	return MemoryAuth{header: http.CanonicalHeaderKey(header), value: value}, nil
}

// NewBearerAuth constructs exact Authorization bearer authentication.
func NewBearerAuth(secret productruntime.SensitiveValue) (MemoryAuth, error) {
	if secret.Empty() || strings.ContainsAny(secret.Reveal(), "\r\n") {
		return MemoryAuth{}, ErrInvalidAuth
	}
	return NewMemoryAuth("Authorization", productruntime.NewSensitiveValue("Bearer "+secret.Reveal()))
}

// NewBasicAuth constructs exact Authorization basic authentication without
// placing the username or password in a URL.
func NewBasicAuth(username string, password productruntime.SensitiveValue) (MemoryAuth, error) {
	if username == "" || strings.ContainsAny(username, ":\r\n") || password.Empty() || strings.ContainsAny(password.Reveal(), "\r\n") {
		return MemoryAuth{}, ErrInvalidAuth
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password.Reveal()))
	return NewMemoryAuth("Authorization", productruntime.NewSensitiveValue("Basic "+encoded))
}

func (auth MemoryAuth) valid() bool {
	return validHeaderName(auth.header) && !auth.value.Empty() && !strings.ContainsAny(auth.value.Reveal(), "\r\n")
}

// Apply overwrites the credential header with the exact in-memory value.
func (auth MemoryAuth) Apply(header http.Header) {
	if header != nil && auth.valid() {
		deleteHeaderFold(header, auth.header)
		header.Set(auth.header, auth.value.Reveal())
	}
}

// Authorize compares one and only one header value in constant time.
func (auth MemoryAuth) Authorize(request *http.Request) bool {
	if request == nil || !auth.valid() {
		return false
	}
	values := headerValuesFold(request.Header, auth.header)
	if len(values) != 1 {
		return false
	}
	expected := []byte(auth.value.Reveal())
	actual := []byte(values[0])
	return len(actual) == len(expected) && subtle.ConstantTimeCompare(actual, expected) == 1
}

func (MemoryAuth) String() string   { return "[REDACTED]" }
func (MemoryAuth) GoString() string { return "[REDACTED]" }
func (MemoryAuth) MarshalJSON() ([]byte, error) {
	return nil, ErrInvalidAuth
}

// ClientConfig configures a direct, no-proxy, no-redirect loopback client.
type ClientConfig struct {
	Endpoint              string
	Auth                  MemoryAuth
	Limits                Limits
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	MaxConnsPerHost       int
}

// Request is a bounded generic HTTP request. Path is an origin-relative path
// (optionally with a query); typed product packages own its meaning.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Response is a fully bounded, optionally decompressed HTTP response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Client is safe for concurrent use.
type Client struct {
	base       *url.URL
	dialAddr   string
	auth       MemoryAuth
	limits     Limits
	httpClient *http.Client
	transport  *http.Transport
}

func NewClient(config ClientConfig) (*Client, error) {
	base, dialAddress, err := validateEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	if !config.Auth.valid() {
		return nil, ErrInvalidAuth
	}
	limits, err := config.Limits.normalized()
	if err != nil {
		return nil, err
	}
	dialTimeout := config.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = defaultDialTimeout
	}
	headerTimeout := config.ResponseHeaderTimeout
	if headerTimeout == 0 {
		headerTimeout = defaultHeaderTimeout
	}
	idleTimeout := config.IdleConnTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultIdleTimeout
	}
	maxConnections := config.MaxConnsPerHost
	if maxConnections == 0 {
		maxConnections = defaultMaxConnsPerHost
	}
	if dialTimeout < 0 || headerTimeout < 0 || idleTimeout < 0 || maxConnections < 1 {
		return nil, ErrInvalidLimits
	}
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            pinnedDialer(dialer, dialAddress),
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		MaxConnsPerHost:        maxConnections,
		MaxIdleConnsPerHost:    maxConnections,
		IdleConnTimeout:        idleTimeout,
		ResponseHeaderTimeout:  headerTimeout,
		MaxResponseHeaderBytes: limits.MaxHeaderBytes,
	}
	httpClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectRefused
		},
	}
	return &Client{
		base: base, dialAddr: dialAddress, auth: config.Auth, limits: limits,
		httpClient: httpClient, transport: transport,
	}, nil
}

func pinnedDialer(dialer *net.Dialer, expected string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, ErrNonLoopback
		}
		if !sameTCPAddress(address, expected) {
			return nil, ErrNonLoopback
		}
		return dialer.DialContext(ctx, network, expected)
	}
}

func sameTCPAddress(left, right string) bool {
	leftHost, leftPort, leftErr := net.SplitHostPort(left)
	rightHost, rightPort, rightErr := net.SplitHostPort(right)
	if leftErr != nil || rightErr != nil || leftPort != rightPort {
		return false
	}
	leftIP, rightIP := net.ParseIP(leftHost), net.ParseIP(rightHost)
	return leftIP != nil && rightIP != nil && leftIP.Equal(rightIP)
}

func validateEndpoint(endpoint string) (*url.URL, string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, "", ErrNonLoopback
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, "", ErrNonLoopback
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, "", ErrNonLoopback
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
	parsed.Host = net.JoinHostPort(ip.String(), port)
	return parsed, net.JoinHostPort(ip.String(), port), nil
}

// Do executes and fully bounds one generic request and response.
func (client *Client) Do(ctx context.Context, request Request) (Response, error) {
	response, err := client.doRaw(ctx, request)
	if err != nil {
		return Response{}, err
	}
	defer response.Body.Close()
	body, err := readResponseBody(response, client.limits)
	if err != nil {
		return Response{}, err
	}
	header := response.Header.Clone()
	if normalizedEncoding(response.Header) == "gzip" {
		header.Del("Content-Encoding")
		header.Del("Content-Length")
	}
	return Response{StatusCode: response.StatusCode, Header: header, Body: body}, nil
}

func (client *Client) doRaw(ctx context.Context, request Request) (*http.Response, error) {
	if client == nil || client.httpClient == nil || ctx == nil {
		return nil, ErrInvalidRequestTarget
	}
	if int64(len(request.Body)) > client.limits.MaxRequestBodyBytes || int64(len(request.Body)) > client.limits.MaxRequestWireBytes {
		return nil, ErrRequestTooLarge
	}
	if request.Method == "" {
		return nil, ErrInvalidRequestTarget
	}
	if int64(len(request.Method)+len(request.Path)) > client.limits.MaxHeaderBytes {
		return nil, ErrHeadersTooLarge
	}
	target, err := client.requestURL(request.Path)
	if err != nil {
		return nil, err
	}
	header := request.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	if len(headerValuesFold(header, client.auth.header)) != 0 {
		return nil, ErrAuthConflict
	}
	if headerSize(header) > client.limits.MaxHeaderBytes {
		return nil, ErrHeadersTooLarge
	}
	client.auth.Apply(header)
	if headerSize(header) > client.limits.MaxHeaderBytes {
		return nil, ErrHeadersTooLarge
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, target.String(), bytes.NewReader(request.Body))
	if err != nil {
		return nil, ErrInvalidRequestTarget
	}
	httpRequest.Header = header
	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		if errors.Is(err, ErrRedirectRefused) {
			return nil, ErrRedirectRefused
		}
		return nil, fmt.Errorf("product server request failed: %w", err)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return nil, ErrRedirectRefused
	}
	return response, nil
}

func (client *Client) requestURL(path string) (*url.URL, error) {
	if path == "" {
		path = "/"
	}
	parsed, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrInvalidRequestTarget
	}
	target := *client.base
	target.Path, target.RawPath, target.RawQuery = parsed.Path, parsed.RawPath, parsed.RawQuery
	return &target, nil
}

func readResponseBody(response *http.Response, limits Limits) ([]byte, error) {
	if response.ContentLength > limits.MaxResponseWireBytes {
		return nil, ErrResponseTooLarge
	}
	wire, err := readBounded(response.Body, limits.MaxResponseWireBytes, ErrResponseTooLarge)
	if err != nil {
		return nil, err
	}
	encoding := normalizedEncoding(response.Header)
	switch encoding {
	case "", "identity":
		if int64(len(wire)) > limits.MaxResponseBodyBytes {
			return nil, ErrResponseTooLarge
		}
		return wire, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil, ErrInvalidResponse
		}
		decompressed, readErr := readBounded(reader, limits.MaxResponseBodyBytes, ErrResponseTooLarge)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, ErrInvalidResponse
		}
		return decompressed, nil
	default:
		return nil, ErrUnsupportedEncoding
	}
}

func readBounded(reader io.Reader, maximum int64, tooLarge error) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, tooLarge
	}
	return body, nil
}

func normalizedEncoding(header http.Header) string {
	values := header.Values("Content-Encoding")
	if len(values) == 0 {
		return ""
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "multiple"
	}
	return strings.ToLower(strings.TrimSpace(values[0]))
}

func headerSize(header http.Header) int64 {
	var size int64
	for name, values := range header {
		size += int64(len(name))
		for _, value := range values {
			size += int64(len(value))
		}
	}
	return size
}

func headerValuesFold(header http.Header, wanted string) []string {
	var values []string
	for name, candidates := range header {
		if strings.EqualFold(name, wanted) {
			values = append(values, candidates...)
		}
	}
	return values
}

func deleteHeaderFold(header http.Header, wanted string) {
	for name := range header {
		if strings.EqualFold(name, wanted) {
			delete(header, name)
		}
	}
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character))) {
			return false
		}
	}
	return true
}

// CloseIdleConnections releases idle direct connections. Active requests are
// controlled by their contexts.
func (client *Client) CloseIdleConnections() {
	if client != nil && client.transport != nil {
		client.transport.CloseIdleConnections()
	}
}

var _ json.Marshaler = MemoryAuth{}
