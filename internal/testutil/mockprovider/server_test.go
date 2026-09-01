package mockprovider_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/testutil/mockprovider"
)

func TestStreamingTextIsDeterministicAndCaptured(t *testing.T) {
	server := mockprovider.Start(t, mockprovider.Script{
		Model:   "fixture-model",
		Created: 1_700_000_123,
		Turns: []mockprovider.Turn{
			{Name: "greeting", Text: []string{"hello", " world"}},
		},
	})

	response := postCompletion(t, context.Background(), server, `{
		"model":"client-model",
		"stream":true,
		"messages":[{"role":"user","content":"say hello"}]
	}`)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, readAll(t, response.Body))
	}
	frames := readSSE(t, response.Body)
	if got, want := textFromFrames(t, frames), "hello world"; got != want {
		t.Fatalf("streamed text = %q, want %q", got, want)
	}
	if got := finishReason(t, frames); got != "stop" {
		t.Fatalf("finish reason = %q, want stop", got)
	}
	if got := idsFromFrames(t, frames); len(got) == 0 || got[0] != "chatcmpl-mock-000001" {
		t.Fatalf("completion IDs = %v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	captured, err := server.WaitRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Sequence != 1 || captured.Turn != "greeting" || !captured.Stream {
		t.Fatalf("captured request = %#v", captured)
	}
	if got := captured.Body["model"]; got != "client-model" {
		t.Fatalf("captured model = %#v", got)
	}

	models, err := server.Client().Get(server.BaseURL() + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer models.Body.Close()
	if models.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d", models.StatusCode)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(models.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Data) != 1 || listing.Data[0].ID != "fixture-model" {
		t.Fatalf("model listing = %#v", listing)
	}
}

func TestToolCallsSupportStreamingAndJSONResponses(t *testing.T) {
	server := mockprovider.Start(t, mockprovider.Script{Turns: []mockprovider.Turn{
		{
			Name: "stream-call",
			ToolCalls: []mockprovider.ToolCall{{
				ID: "call_stream", Name: "send_message", Arguments: `{"peer":"fable","text":"hello"}`,
			}},
		},
		{
			Name: "json-call",
			ToolCalls: []mockprovider.ToolCall{{
				ID: "call_json", Name: "read_file", Arguments: `{"path":"README.md"}`,
			}},
		},
	}})

	streamed := postCompletion(t, context.Background(), server, `{"stream":true,"messages":[]}`)
	frames := readSSE(t, streamed.Body)
	streamed.Body.Close()
	if got := finishReason(t, frames); got != "tool_calls" {
		t.Fatalf("stream finish reason = %q", got)
	}
	call := firstStreamToolCall(t, frames)
	if call.ID != "call_stream" || call.Function.Name != "send_message" || call.Function.Arguments != `{"peer":"fable","text":"hello"}` {
		t.Fatalf("stream tool call = %#v", call)
	}

	plain := postCompletion(t, context.Background(), server, `{"stream":false,"messages":[]}`)
	defer plain.Body.Close()
	var completion struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				ToolCalls []wireToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	decodeJSON(t, plain.Body, &completion)
	if completion.ID != "chatcmpl-mock-000002" || len(completion.Choices) != 1 {
		t.Fatalf("completion = %#v", completion)
	}
	if completion.Choices[0].FinishReason != "tool_calls" || len(completion.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("completion choices = %#v", completion.Choices)
	}
	if got := completion.Choices[0].Message.ToolCalls[0]; got.ID != "call_json" || got.Function.Name != "read_file" {
		t.Fatalf("JSON tool call = %#v", got)
	}
}

func TestBlockedTurnsHaveIndependentReleaseControls(t *testing.T) {
	server := mockprovider.Start(t, mockprovider.Script{Turns: []mockprovider.Turn{
		{Name: "alpha", Text: []string{"ALPHA"}, Block: true},
		{Name: "beta", Text: []string{"BETA"}, Block: true},
	}})

	type result struct {
		prompt string
		text   string
		err    error
	}
	results := make(chan result, 2)
	for _, prompt := range []string{"one", "two"} {
		prompt := prompt
		go func() {
			response, err := doCompletion(context.Background(), server, fmt.Sprintf(`{"stream":true,"messages":[{"role":"user","content":%q}]}`, prompt))
			if err != nil {
				results <- result{prompt: prompt, err: err}
				return
			}
			defer response.Body.Close()
			frames, err := scanSSE(response.Body)
			if err != nil {
				results <- result{prompt: prompt, err: err}
				return
			}
			text, err := textFromFramesE(frames)
			results <- result{prompt: prompt, text: text, err: err}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := server.WaitRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.WaitRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence == second.Sequence {
		t.Fatalf("duplicate captured sequence %d", first.Sequence)
	}
	if !server.Release(first.Sequence) {
		t.Fatalf("release first request %d failed", first.Sequence)
	}
	gotFirst := <-results
	wantFirst := map[string]string{"alpha": "ALPHA", "beta": "BETA"}[first.Turn]
	if gotFirst.err != nil || gotFirst.text != wantFirst {
		t.Fatalf("first result = %#v, captured = %#v", gotFirst, first)
	}
	if !server.Release(second.Sequence) {
		t.Fatalf("release second request %d failed", second.Sequence)
	}
	gotSecond := <-results
	wantSecond := map[string]string{"alpha": "ALPHA", "beta": "BETA"}[second.Turn]
	if gotSecond.err != nil || gotSecond.text != wantSecond {
		t.Fatalf("second result = %#v, captured = %#v", gotSecond, second)
	}
	if server.Release(first.Sequence) {
		t.Fatal("released the same request twice")
	}
}

func TestCanceledBlockedTurnIsObserved(t *testing.T) {
	server := mockprovider.Start(t, mockprovider.Script{Turns: []mockprovider.Turn{
		{Name: "slow", Text: []string{"too late"}, Block: true},
	}})

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		response, err := doCompletion(requestCtx, server, `{"stream":true,"messages":[]}`)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	captured, err := server.WaitRequest(waitCtx)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("client error = %v, want context canceled", err)
	}
	cancellation, err := server.WaitCancellation(waitCtx)
	if err != nil {
		t.Fatal(err)
	}
	if cancellation.Sequence != captured.Sequence || cancellation.Turn != "slow" || cancellation.Cause != "context canceled" {
		t.Fatalf("cancellation = %#v, captured = %#v", cancellation, captured)
	}
	if server.Release(captured.Sequence) {
		t.Fatal("canceled request remained releasable")
	}
}

func TestHTTPErrorMalformedStreamAndDisconnectScripts(t *testing.T) {
	server := mockprovider.Start(t, mockprovider.Script{Turns: []mockprovider.Turn{
		{
			Name: "rate-limit",
			Error: &mockprovider.ErrorResponse{
				Status: http.StatusTooManyRequests, Code: "rate_limit", Type: "rate_limit_error", Message: "try later",
			},
		},
		{Name: "malformed", MalformedStream: []string{"{not-json"}},
		{Name: "disconnect", Disconnect: true},
	}})

	failed := postCompletion(t, context.Background(), server, `{"messages":[]}`)
	if failed.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("HTTP error status = %d", failed.StatusCode)
	}
	var errorEnvelope struct {
		Error struct{ Code, Type, Message string } `json:"error"`
	}
	decodeJSON(t, failed.Body, &errorEnvelope)
	failed.Body.Close()
	if errorEnvelope.Error.Code != "rate_limit" || errorEnvelope.Error.Type != "rate_limit_error" || errorEnvelope.Error.Message != "try later" {
		t.Fatalf("HTTP error = %#v", errorEnvelope.Error)
	}

	malformed := postCompletion(t, context.Background(), server, `{"stream":true,"messages":[]}`)
	frames := readSSE(t, malformed.Body)
	malformed.Body.Close()
	if len(frames) != 1 || frames[0] != "{not-json" {
		t.Fatalf("malformed frames = %q", frames)
	}

	response, err := doCompletion(context.Background(), server, `{"stream":true,"messages":[]}`)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("disconnect turn unexpectedly returned a response")
	}
}

func TestInputAndCaptureBounds(t *testing.T) {
	server := mockprovider.Start(t, mockprovider.Script{Turns: []mockprovider.Turn{
		{Text: []string{"first"}},
		{Text: []string{"second"}},
	}}, mockprovider.WithLimits(mockprovider.Limits{
		MaxRequestBytes:     96,
		MaxCapturedRequests: 1,
		MaxMessages:         2,
		MaxTools:            1,
	}))

	oversized := postCompletion(t, context.Background(), server, `{"messages":[{"role":"user","content":"`+strings.Repeat("x", 100)+`"}]}`)
	if oversized.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, body = %s", oversized.StatusCode, readAll(t, oversized.Body))
	}
	oversized.Body.Close()
	if got := len(server.Requests()); got != 0 {
		t.Fatalf("captured oversized requests = %d", got)
	}

	tooManyMessages := postCompletion(t, context.Background(), server, `{"messages":[{},{},{}]}`)
	if tooManyMessages.StatusCode != http.StatusBadRequest {
		t.Fatalf("too-many-messages status = %d", tooManyMessages.StatusCode)
	}
	tooManyMessages.Body.Close()
	if got := len(server.Requests()); got != 0 {
		t.Fatalf("captured invalid requests = %d", got)
	}

	accepted := postCompletion(t, context.Background(), server, `{"messages":[]}`)
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("accepted status = %d", accepted.StatusCode)
	}
	accepted.Body.Close()

	rejected := postCompletion(t, context.Background(), server, `{"messages":[]}`)
	if rejected.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("capture-bound status = %d, body = %s", rejected.StatusCode, readAll(t, rejected.Body))
	}
	rejected.Body.Close()
	requests := server.Requests()
	if len(requests) != 1 || requests[0].Sequence != 1 {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestCapturedCredentialsAreRedactedAndSnapshotsAreIsolated(t *testing.T) {
	server := mockprovider.Start(t, mockprovider.Script{Turns: []mockprovider.Turn{{Text: []string{"ok"}}}})
	request, err := http.NewRequest(http.MethodPost, server.BaseURL()+"/chat/completions?api_key=query-secret&trace=keep", strings.NewReader(`{
		"messages":[],
		"api_key":"body-secret",
		"client":{"access_token":"nested-secret","label":"keep"},
		"signing":{"private_key":"key-secret"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer header-secret")
	request.Header.Set("X-Api-Key", "other-header-secret")
	request.Header.Set("X-Secret-Token", "custom-header-secret")
	request.Header.Set("X-Trace", "keep")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	requests := server.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	captured := requests[0]
	if got := captured.Header.Get("Authorization"); got != mockprovider.Redacted {
		t.Fatalf("Authorization = %q", got)
	}
	if got := captured.Header.Get("X-Api-Key"); got != mockprovider.Redacted {
		t.Fatalf("X-Api-Key = %q", got)
	}
	if got := captured.Header.Get("X-Secret-Token"); got != mockprovider.Redacted {
		t.Fatalf("X-Secret-Token = %q", got)
	}
	if got := captured.Header.Get("X-Trace"); got != "keep" {
		t.Fatalf("X-Trace = %q", got)
	}
	if got := captured.Query.Get("api_key"); got != mockprovider.Redacted {
		t.Fatalf("query api_key = %q", got)
	}
	if got := captured.Query.Get("trace"); got != "keep" {
		t.Fatalf("query trace = %q", got)
	}
	if got := captured.Body["api_key"]; got != mockprovider.Redacted {
		t.Fatalf("body api_key = %#v", got)
	}
	client, ok := captured.Body["client"].(map[string]any)
	if !ok || client["access_token"] != mockprovider.Redacted || client["label"] != "keep" {
		t.Fatalf("captured client = %#v", captured.Body["client"])
	}
	signing, ok := captured.Body["signing"].(map[string]any)
	if !ok || signing["private_key"] != mockprovider.Redacted {
		t.Fatalf("captured signing material = %#v", captured.Body["signing"])
	}

	// Mutating a returned snapshot must not mutate the fixture's retained copy.
	captured.Header.Set("Authorization", "restored-secret")
	captured.Query.Set("api_key", "restored-secret")
	client["access_token"] = "restored-secret"
	again := server.Requests()[0]
	if again.Header.Get("Authorization") != mockprovider.Redacted || again.Query.Get("api_key") != mockprovider.Redacted {
		t.Fatalf("retained request was mutated: %#v", again)
	}
	againClient := again.Body["client"].(map[string]any)
	if againClient["access_token"] != mockprovider.Redacted {
		t.Fatalf("retained body was mutated: %#v", againClient)
	}
}

func TestConcurrentRequestsKeepTurnStateIsolated(t *testing.T) {
	const requestCount = 32
	turns := make([]mockprovider.Turn, requestCount)
	for i := range turns {
		turns[i] = mockprovider.Turn{Name: fmt.Sprintf("turn-%02d", i+1), Text: []string{fmt.Sprintf("TEXT-%02d", i+1)}}
	}
	server := mockprovider.Start(t, mockprovider.Script{Turns: turns}, mockprovider.WithLimits(mockprovider.Limits{
		MaxRequestBytes:     1024,
		MaxCapturedRequests: requestCount,
		MaxMessages:         4,
		MaxTools:            4,
	}))

	type result struct {
		prompt string
		text   string
		err    error
	}
	results := make(chan result, requestCount)
	var ready sync.WaitGroup
	ready.Add(requestCount)
	start := make(chan struct{})
	for i := 0; i < requestCount; i++ {
		prompt := fmt.Sprintf("request-%02d", i+1)
		go func() {
			ready.Done()
			<-start
			response, err := doCompletion(context.Background(), server, fmt.Sprintf(`{"stream":true,"messages":[{"role":"user","content":%q}]}`, prompt))
			if err != nil {
				results <- result{prompt: prompt, err: err}
				return
			}
			defer response.Body.Close()
			frames, err := scanSSE(response.Body)
			if err != nil {
				results <- result{prompt: prompt, err: err}
				return
			}
			text, err := textFromFramesE(frames)
			results <- result{prompt: prompt, text: text, err: err}
		}()
	}
	ready.Wait()
	close(start)

	gotByPrompt := make(map[string]string, requestCount)
	for i := 0; i < requestCount; i++ {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		gotByPrompt[got.prompt] = got.text
	}
	requests := server.Requests()
	if len(requests) != requestCount {
		t.Fatalf("captured %d requests, want %d", len(requests), requestCount)
	}
	seenSequences := make(map[uint64]bool, requestCount)
	for _, captured := range requests {
		messages := captured.Body["messages"].([]any)
		prompt := messages[0].(map[string]any)["content"].(string)
		want := strings.Replace(captured.Turn, "turn-", "TEXT-", 1)
		if got := gotByPrompt[prompt]; got != want {
			t.Fatalf("request %q received %q, want %q for %#v", prompt, got, want, captured)
		}
		if seenSequences[captured.Sequence] {
			t.Fatalf("duplicate sequence %d", captured.Sequence)
		}
		seenSequences[captured.Sequence] = true
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		script mockprovider.Script
		opts   []mockprovider.Option
	}{
		{name: "no turns", script: mockprovider.Script{}},
		{name: "bad tool arguments", script: mockprovider.Script{Turns: []mockprovider.Turn{{ToolCalls: []mockprovider.ToolCall{{Name: "call", Arguments: "{"}}}}}},
		{name: "bad error status", script: mockprovider.Script{Turns: []mockprovider.Turn{{Error: &mockprovider.ErrorResponse{Status: 200}}}}},
		{name: "conflicting terminal modes", script: mockprovider.Script{Turns: []mockprovider.Turn{{Text: []string{"x"}, Disconnect: true}}}},
		{name: "bad bounds", script: mockprovider.Script{Turns: []mockprovider.Turn{{Text: []string{"x"}}}}, opts: []mockprovider.Option{mockprovider.WithLimits(mockprovider.Limits{MaxRequestBytes: -1})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := mockprovider.New(test.script, test.opts...)
			if server != nil {
				server.Close()
			}
			if err == nil {
				t.Fatal("New succeeded")
			}
		})
	}
}

type wireToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func postCompletion(t *testing.T, ctx context.Context, server *mockprovider.Server, body string) *http.Response {
	t.Helper()
	response, err := doCompletion(ctx, server, body)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func doCompletion(ctx context.Context, server *mockprovider.Server, body string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.BaseURL()+"/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return server.Client().Do(request)
}

func readAll(t *testing.T, reader io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeJSON(t *testing.T, reader io.Reader, target any) {
	t.Helper()
	if err := json.NewDecoder(reader).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func readSSE(t *testing.T, reader io.Reader) []string {
	t.Helper()
	frames, err := scanSSE(reader)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func scanSSE(reader io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(reader)
	frames := make([]string, 0)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		frames = append(frames, payload)
	}
	return frames, scanner.Err()
}

func textFromFrames(t *testing.T, frames []string) string {
	t.Helper()
	text, err := textFromFramesE(frames)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func textFromFramesE(frames []string) (string, error) {
	var text strings.Builder
	for _, frame := range frames {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(bytes.NewBufferString(frame)).Decode(&chunk); err != nil {
			return "", err
		}
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}
	return text.String(), nil
}

func finishReason(t *testing.T, frames []string) string {
	t.Helper()
	for _, frame := range frames {
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				return *choice.FinishReason
			}
		}
	}
	return ""
}

func idsFromFrames(t *testing.T, frames []string) []string {
	t.Helper()
	ids := make([]string, 0, len(frames))
	for _, frame := range frames {
		var chunk struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, chunk.ID)
	}
	return ids
}

func firstStreamToolCall(t *testing.T, frames []string) wireToolCall {
	t.Helper()
	for _, frame := range frames {
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []wireToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, choice := range chunk.Choices {
			if len(choice.Delta.ToolCalls) > 0 {
				return choice.Delta.ToolCalls[0]
			}
		}
	}
	t.Fatal("no streamed tool call")
	return wireToolCall{}
}
