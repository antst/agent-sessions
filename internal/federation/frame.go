package federation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AgentFrameCarrierPrefix marks the JSON body in native text carriers.
const AgentFrameCarrierPrefix = "AGENT_SESSIONS_FRAME "

// AgentFrame is the product-neutral peer-message wire payload.
type AgentFrame struct {
	Version         int      `json:"version"`
	Type            string   `json:"type"`
	MessageID       string   `json:"message_id"`
	SourceSessionID string   `json:"source_session_id,omitempty"`
	Source          *Peer    `json:"source,omitempty"`
	Targets         []string `json:"targets,omitempty"`
	Group           string   `json:"group,omitempty"`
	Content         string   `json:"content,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	SentAt          string   `json:"sent_at,omitempty"`
}

// DeliveryResult is one exact destination outcome.
type DeliveryResult struct {
	Target    string `json:"target"`
	SessionID string `json:"session_id,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// AgentFrameResult is the correlated result of an AgentFrame operation.
type AgentFrameResult struct {
	Version    int              `json:"version"`
	Type       string           `json:"type"`
	MessageID  string           `json:"message_id,omitempty"`
	Peers      []Peer           `json:"peers,omitempty"`
	Deliveries []DeliveryResult `json:"deliveries,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// DecodeAgentFrameBody unwraps a native carrier and decodes its AgentFrame.
func DecodeAgentFrameBody(content string) (AgentFrame, error) {
	body, err := UnwrapAgentFrameCarrierBody(content)
	if err != nil {
		return AgentFrame{}, err
	}
	body = strings.TrimPrefix(body, AgentFrameCarrierPrefix)
	var frame AgentFrame
	if err := json.Unmarshal([]byte(body), &frame); err != nil {
		return AgentFrame{}, fmt.Errorf("decode agent frame: %w", err)
	}
	return frame, nil
}

// UnwrapAgentFrameCarrierBody removes the optional Claude carrier envelope.
func UnwrapAgentFrameCarrierBody(content string) (string, error) {
	body := content
	if strings.HasPrefix(body, "<cross-session-message") {
		newline := strings.IndexByte(body, '\n')
		const closeTag = "\n</cross-session-message>"
		if newline < 0 || !strings.HasSuffix(body, closeTag) {
			return "", errors.New("invalid Claude carrier envelope")
		}
		body = body[newline+1 : len(body)-len(closeTag)]
	}
	return body, nil
}

// EscapeAgentFrameEnvelopeJSON prevents a JSON string from closing its carrier.
func EscapeAgentFrameEnvelopeJSON(body []byte) []byte {
	lower := bytes.ToLower(body)
	needle := []byte("</cross-session-message")
	for {
		index := bytes.Index(lower, needle)
		if index < 0 {
			return body
		}
		body = append(append(append([]byte(nil), body[:index+1]...), '\\'), body[index+1:]...)
		lower = bytes.ToLower(body)
	}
}
