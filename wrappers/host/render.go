package host

import (
	"encoding/json"
	"errors"
	"strings"

	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
)

func render(request sessionkit.DeliveryRequest) (string, error) {
	if strings.TrimSpace(request.From.SessionID) == "" || strings.TrimSpace(request.From.Product) == "" {
		return "", errors.New("structured message sender is incomplete")
	}
	from := request.From.Name
	if from == "" {
		from = request.From.SessionID
	}
	attributes := `from="` + safeAttribute(from) + `" from-session="` + safeAttribute(request.From.SessionID) + `"`
	metadata, err := json.Marshal(struct {
		FromProduct string   `json:"fromProduct"`
		MessageID   string   `json:"messageId"`
		Groups      []string `json:"groups"`
	}{request.From.Product, request.MessageID, request.From.Groups})
	if err != nil {
		return "", err
	}
	return "<cross-session-message " + attributes + ">\n[agentbus-metadata: " + string(metadata) + "]\n" +
		escapeEnvelopeBody(request.Body) + "\n</cross-session-message>", nil
}

func safeAttribute(value string) string {
	return strings.NewReplacer(`"`, "", "<", "", ">", "", "\n", "", "\r", "").Replace(value)
}

func escapeEnvelopeBody(message string) string {
	const closing = "</cross-session-message"
	lower := strings.ToLower(message)
	var output strings.Builder
	for {
		index := strings.Index(lower, closing)
		if index < 0 {
			output.WriteString(message)
			return output.String()
		}
		end := index + len(closing)
		output.WriteString(message[:index])
		output.WriteString("<\\/cross-session-message")
		message, lower = message[end:], lower[end:]
	}
}
