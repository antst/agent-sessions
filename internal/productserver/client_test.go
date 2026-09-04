package productserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestClientSendsAuthenticatedProductRequest(t *testing.T) {
	auth, err := NewBearerAuth(productruntime.NewSensitiveValue("native-token"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !auth.Authorize(request) || request.URL.Path != "/session" || request.URL.Query().Get("id") != "native" {
			http.Error(response, "bad request", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"text":"hello"}` {
			http.Error(response, "bad body", http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{Endpoint: server.URL, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Do(context.Background(), Request{
		Method: http.MethodPost,
		Path:   "/session?id=native",
		Body:   []byte(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK || string(result.Body) != `{"ok":true}` {
		t.Fatalf("response = %#v", result)
	}
}

func TestClientKeepsSimpleBodyBound(t *testing.T) {
	auth, _ := NewMemoryAuth("X-Test", productruntime.NewSensitiveValue("ok"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("too large"))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, Auth: auth, Limits: Limits{MaxResponseBodyBytes: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/"}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("bounded response = %v", err)
	}
}

func TestClientRejectsNonLoopbackEndpointBeforeAuth(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "http://localhost.example"} {
		if _, err := NewClient(ClientConfig{Endpoint: endpoint}); !errors.Is(err, ErrNonLoopback) {
			t.Fatalf("NewClient(%q) error = %v", endpoint, err)
		}
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	auth, err := NewMemoryAuth("X-Test", productruntime.NewSensitiveValue("ok"))
	if err != nil {
		t.Fatal(err)
	}
	targetCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/target" {
			targetCalls++
			response.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(response, request, "/target", http.StatusFound)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), Request{Method: http.MethodGet, Path: "/start"})
	var redirectErr *url.Error
	if !errors.As(err, &redirectErr) || !errors.Is(err, ErrNonLoopback) {
		t.Fatalf("redirect error = %T %v", err, err)
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target calls = %d", targetCalls)
	}
}
