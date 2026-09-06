package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const nativeLimit = 1 << 20

type nativeClient struct {
	endpoint, username, password, directory string
	http                                    *http.Client
}

type nativeSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
}

type admission struct {
	Sequence  *int64 `json:"admittedSeq"`
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Prompt    struct {
		Text string `json:"text"`
	} `json:"prompt"`
	Delivery string `json:"delivery"`
}

type nativeMessage struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Finish string          `json:"finish"`
	Error  json.RawMessage `json:"error"`
	Time   struct {
		Completed *float64 `json:"completed"`
	} `json:"time"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (c *nativeClient) request(ctx context.Context, method, path string, body any, expected ...int) ([]byte, error) {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, input)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(c.username, c.password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, nativeLimit+1)
	result, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(result) > nativeLimit {
		return nil, errors.New("OpenCode response exceeds 1 MiB")
	}
	for _, status := range expected {
		if response.StatusCode == status {
			return result, nil
		}
	}
	return nil, fmt.Errorf("OpenCode %s %s returned HTTP %d", method, path, response.StatusCode)
}

func (c *nativeClient) scoped(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + url.Values{"directory": []string{c.directory}}.Encode()
}

func (c *nativeClient) ready(ctx context.Context) (bool, error) {
	body, err := c.request(ctx, http.MethodGet, "/doc", nil, http.StatusOK)
	if err != nil {
		return false, err
	}
	var document struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if json.Unmarshal(body, &document) != nil {
		return false, errors.New("OpenCode /doc is malformed")
	}
	for _, path := range []string{"/session", "/session/{sessionID}", "/event", "/api/session/{sessionID}/prompt", "/api/session/{sessionID}/wait", "/api/session/{sessionID}/message", "/api/session/{sessionID}/interrupt", "/api/session/{sessionID}/model", "/api/session/{sessionID}/agent", "/experimental/tool/ids"} {
		if _, found := document.Paths[path]; !found {
			return false, fmt.Errorf("OpenCode is missing route %s", path)
		}
	}
	body, err = c.request(ctx, http.MethodGet, c.scoped("/experimental/tool/ids"), nil, http.StatusOK)
	if err != nil {
		return false, err
	}
	var tools []string
	if json.Unmarshal(body, &tools) != nil || tools == nil {
		return false, errors.New("OpenCode tool inventory is malformed")
	}
	for _, name := range tools {
		if name == ToolName {
			return true, nil
		}
	}
	return false, nil
}

func (c *nativeClient) create(ctx context.Context, title, permission string) (nativeSession, error) {
	result, err := c.request(ctx, http.MethodPost, c.scoped("/session"), sessionUpdate(title, permission), http.StatusOK)
	if err != nil {
		return nativeSession{}, err
	}
	var session nativeSession
	if json.Unmarshal(result, &session) != nil || !validNativeID(session.ID) || session.Title != title || session.Directory != c.directory {
		return nativeSession{}, errors.New("OpenCode created an ambiguous session")
	}
	return session, nil
}

func (c *nativeClient) update(ctx context.Context, id, title, permission string) (nativeSession, error) {
	result, err := c.request(ctx, http.MethodPatch, c.scoped("/session/"+url.PathEscape(id)), sessionUpdate(title, permission), http.StatusOK)
	if err != nil {
		return nativeSession{}, err
	}
	var session nativeSession
	if json.Unmarshal(result, &session) != nil || session.ID != id || session.Title != title || session.Directory != c.directory {
		return nativeSession{}, errors.New("OpenCode did not confirm resumed session settings")
	}
	return session, nil
}

func sessionUpdate(title, permission string) map[string]any {
	return map[string]any{"title": title, "permission": []map[string]string{{"permission": "*", "pattern": "*", "action": permission}}}
}

func (c *nativeClient) get(ctx context.Context, id string) (nativeSession, error) {
	result, err := c.request(ctx, http.MethodGet, c.scoped("/session/"+url.PathEscape(id)), nil, http.StatusOK)
	if err != nil {
		return nativeSession{}, err
	}
	var session nativeSession
	if json.Unmarshal(result, &session) != nil || session.ID != id || session.Directory != c.directory {
		return nativeSession{}, errors.New("OpenCode returned a different session")
	}
	return session, nil
}

func (c *nativeClient) resume(ctx context.Context, id, title, permission string) (nativeSession, error) {
	if _, err := c.get(ctx, id); err != nil {
		return nativeSession{}, err
	}
	return c.update(ctx, id, title, permission)
}

func (c *nativeClient) configure(ctx context.Context, id string, model *modelRef, agent string) error {
	if model != nil {
		if _, err := c.request(ctx, http.MethodPost, "/api/session/"+url.PathEscape(id)+"/model", map[string]any{"model": model}, http.StatusNoContent); err != nil {
			return err
		}
	}
	if agent != "" {
		_, err := c.request(ctx, http.MethodPost, "/api/session/"+url.PathEscape(id)+"/agent", map[string]string{"agent": agent}, http.StatusNoContent)
		return err
	}
	return nil
}

func (c *nativeClient) prompt(ctx context.Context, id, messageID, text, delivery string, resume bool) (admission, error) {
	body := map[string]any{"id": messageID, "prompt": map[string]string{"text": text}, "delivery": delivery, "resume": resume}
	result, err := c.request(ctx, http.MethodPost, "/api/session/"+url.PathEscape(id)+"/prompt", body, http.StatusOK)
	if err != nil {
		return admission{}, err
	}
	var envelope struct {
		Data admission `json:"data"`
	}
	if json.Unmarshal(result, &envelope) != nil || envelope.Data.Sequence == nil || *envelope.Data.Sequence < 0 || envelope.Data.ID != messageID || envelope.Data.SessionID != id || envelope.Data.Prompt.Text != text || envelope.Data.Delivery != delivery {
		return admission{}, errors.New("OpenCode returned an invalid input admission")
	}
	return envelope.Data, nil
}

func (c *nativeClient) wait(ctx context.Context, id string) error {
	_, err := c.request(ctx, http.MethodPost, "/api/session/"+url.PathEscape(id)+"/wait", nil, http.StatusNoContent)
	return err
}

func (c *nativeClient) interrupt(ctx context.Context, id string) error {
	_, err := c.request(ctx, http.MethodPost, "/api/session/"+url.PathEscape(id)+"/interrupt", nil, http.StatusNoContent)
	return err
}

func (c *nativeClient) remove(ctx context.Context, id string) error {
	result, err := c.request(ctx, http.MethodDelete, c.scoped("/session/"+url.PathEscape(id)), nil, http.StatusOK)
	if err != nil {
		return err
	}
	var deleted bool
	if json.Unmarshal(result, &deleted) != nil || !deleted {
		return errors.New("OpenCode did not confirm session deletion")
	}
	return nil
}

func (c *nativeClient) rejectPermission(ctx context.Context, sessionID, permissionID string) error {
	if !validNativeID(sessionID) || !validPermissionID(permissionID) {
		return errors.New("OpenCode permission event is malformed")
	}
	result, err := c.request(ctx, http.MethodPost, c.scoped("/session/"+url.PathEscape(sessionID)+"/permissions/"+url.PathEscape(permissionID)), map[string]string{"response": "reject"}, http.StatusOK)
	if err != nil {
		return err
	}
	var rejected bool
	if json.Unmarshal(result, &rejected) != nil || !rejected {
		return errors.New("OpenCode did not confirm permission rejection")
	}
	return nil
}

func (c *nativeClient) result(ctx context.Context, sessionID, messageID string) (string, string, error) {
	base := "/api/session/" + url.PathEscape(sessionID) + "/message"
	path := base + "?order=asc&limit=100"
	found, answered, count := false, false, 0
	seen := map[string]bool{}
	var output strings.Builder
	var stop string
	for path != "" {
		body, err := c.request(ctx, http.MethodGet, path, nil, http.StatusOK)
		if err != nil {
			return "", "", err
		}
		var page struct {
			Data   []nativeMessage `json:"data"`
			Cursor struct {
				Next string `json:"next"`
			} `json:"cursor"`
		}
		if json.Unmarshal(body, &page) != nil || count+len(page.Data) > 4096 {
			return "", "", errors.New("OpenCode message history is malformed")
		}
		count += len(page.Data)
		for _, message := range page.Data {
			if !found {
				if message.ID == messageID {
					if message.Type != "user" {
						return "", "", errors.New("OpenCode admitted message changed role")
					}
					found = true
				}
				continue
			}
			if message.Type != "assistant" || message.Time.Completed == nil {
				continue
			}
			answered = true
			trimmed := bytes.TrimSpace(message.Error)
			if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
				return output.String(), message.Finish, fmt.Errorf("OpenCode assistant failed: %s", trimmed)
			}
			var text strings.Builder
			for _, content := range message.Content {
				if content.Type == "text" {
					text.WriteString(content.Text)
				}
			}
			if text.Len() > 0 {
				separator := 0
				if output.Len() > 0 {
					separator = 1
				}
				if output.Len()+separator+text.Len() > nativeLimit {
					return "", "", errors.New("OpenCode result exceeds 1 MiB")
				}
				if separator != 0 {
					output.WriteByte('\n')
				}
				output.WriteString(text.String())
			}
			stop = message.Finish
		}
		if page.Cursor.Next == "" {
			path = ""
		} else {
			if seen[page.Cursor.Next] {
				return "", "", errors.New("OpenCode message history repeated a cursor")
			}
			seen[page.Cursor.Next] = true
			path = base + "?limit=100&cursor=" + url.QueryEscape(page.Cursor.Next)
		}
	}
	if !found {
		return "", "", errors.New("OpenCode idle history omitted the admitted input")
	}
	if !answered {
		return "", "", errors.New("OpenCode idle history omitted a completed assistant")
	}
	return output.String(), stop, nil
}

func (c *nativeClient) subscribe(ctx context.Context, handle func(context.Context, nativeEvent) error) (<-chan error, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+c.scoped("/event"), nil)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(c.username, c.password)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("OpenCode event stream returned HTTP %d", response.StatusCode)
	}
	done := make(chan error, 1)
	go func() {
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), nativeLimit)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event nativeEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil || event.Type == "" {
				done <- errors.New("OpenCode event stream is malformed")
				return
			}
			if err := handle(ctx, event); err != nil {
				done <- err
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			done <- err
			return
		}
		if ctx.Err() != nil {
			done <- ctx.Err()
		} else {
			done <- errors.New("OpenCode event stream ended")
		}
	}()
	return done, nil
}

type nativeEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		ID        string `json:"id"`
	} `json:"properties"`
}

func deliveryMessageID(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "msg_" + hex.EncodeToString(digest[:16])
}

func validNativeID(value string) bool {
	return strings.HasPrefix(value, "ses") && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 128 && !strings.ContainsFunc(value, func(character rune) bool {
		return !unicode.IsPrint(character) || unicode.IsSpace(character)
	})
}

func validPermissionID(value string) bool {
	return strings.HasPrefix(value, "per") && utf8.ValidString(value) && len(value) <= 4096 && !strings.ContainsAny(value, "\x00\r\n")
}

func randomMessageID() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(randReader, value); err != nil {
		return "", err
	}
	return "msg_" + hex.EncodeToString(value), nil
}

var randReader io.Reader = rand.Reader
