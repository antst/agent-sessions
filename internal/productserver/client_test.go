package productserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestClientRejectsEveryNonLiteralLoopbackEndpoint(t *testing.T) {
	auth := testAuth(t)
	for _, endpoint := range []string{
		"http://localhost:1234",
		"http://example.com:1234",
		"http://192.168.1.4:1234",
		"https://127.0.0.1:1234",
		"http://user:password@127.0.0.1:1234",
		"http://127.0.0.1:1234?secret=yes",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := NewClient(ClientConfig{Endpoint: endpoint, Auth: auth}); !errors.Is(err, ErrNonLoopback) {
				t.Fatalf("NewClient(%q) error = %v, want ErrNonLoopback", endpoint, err)
			}
		})
	}
}

func TestClientRefusesRedirectsAndEnvironmentProxy(t *testing.T) {
	auth := testAuth(t)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()

	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		proxied.Add(1)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !auth.Authorize(request) {
			t.Error("request did not carry exact memory auth")
		}
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := newTestClient(t, source.URL, auth, Limits{})

	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/redirect"})
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("redirect error = %v, want ErrRedirectRefused", err)
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
	if got := proxied.Load(); got != 0 {
		t.Fatalf("environment proxy requests = %d, want 0", got)
	}
}

func TestClientBoundsRequestResponseAndGzipDecompression(t *testing.T) {
	auth := testAuth(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/echo":
			body, _ := io.ReadAll(request.Body)
			_, _ = response.Write(body)
		case "/large":
			_, _ = response.Write([]byte("123456789"))
		case "/bomb":
			response.Header().Set("Content-Encoding", "gzip")
			writer := gzip.NewWriter(response)
			_, _ = writer.Write(bytes.Repeat([]byte("z"), 128))
			_ = writer.Close()
		case "/unknown-encoding":
			response.Header().Set("Content-Encoding", "br")
			_, _ = response.Write([]byte("body"))
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, auth, Limits{
		MaxRequestBodyBytes:  8,
		MaxResponseWireBytes: 64,
		MaxResponseBodyBytes: 8,
	})

	if _, err := client.Do(context.Background(), Request{Method: http.MethodPost, Path: "/echo", Body: []byte("123456789")}); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("large request error = %v, want ErrRequestTooLarge", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("oversized request reached server %d times", got)
	}
	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/large"}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("large response error = %v, want ErrResponseTooLarge", err)
	}
	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/bomb"}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("gzip bomb error = %v, want ErrResponseTooLarge", err)
	}
	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/unknown-encoding"}); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("unknown encoding error = %v, want ErrUnsupportedEncoding", err)
	}
}

func TestClientKeepsAuthenticationOutOfURLsErrorsAndJSON(t *testing.T) {
	secret := "memory-secret-that-must-not-leak"
	auth, err := NewBearerAuth(productruntime.NewSensitiveValue(secret))
	if err != nil {
		t.Fatal(err)
	}
	if text := auth.String(); strings.Contains(text, secret) || text != "[REDACTED]" {
		t.Fatalf("auth string = %q", text)
	}
	if _, err := json.Marshal(auth); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("auth JSON error = %v", err)
	}
	if _, err := NewClient(ClientConfig{Endpoint: "http://localhost:42/" + secret, Auth: auth}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("endpoint error leaked auth/path detail: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !auth.Authorize(request) {
			t.Error("exact bearer auth rejected")
		}
		_, _ = response.Write([]byte("ok"))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, auth, Limits{})
	if _, err := client.Do(context.Background(), Request{
		Method: http.MethodGet,
		Path:   "/",
		Header: http.Header{"Authorization": []string{"Bearer attacker"}},
	}); !errors.Is(err, ErrAuthConflict) {
		t.Fatalf("caller-provided auth error = %v, want ErrAuthConflict", err)
	}
	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "http://127.0.0.1:1/escape"}); !errors.Is(err, ErrInvalidRequestTarget) {
		t.Fatalf("absolute target error = %v, want ErrInvalidRequestTarget", err)
	}
}

func newTestClient(t *testing.T, endpoint string, auth MemoryAuth, limits Limits) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{Endpoint: endpoint, Auth: auth, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func testAuth(t *testing.T) MemoryAuth {
	t.Helper()
	auth, err := NewBearerAuth(productruntime.NewSensitiveValue("test-memory-token"))
	if err != nil {
		t.Fatal(err)
	}
	return auth
}
