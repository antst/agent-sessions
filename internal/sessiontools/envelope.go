package sessiontools

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/antst/sessionbus/internal/productruntime"
)

// RenderNativeMessage is the one Go-side rendering of a structured v1
// delivery at a product's native text-input boundary.
func RenderNativeMessage(message productruntime.NativeMessage) (string, error) {
	if strings.TrimSpace(message.From.UUID) == "" || strings.TrimSpace(message.From.Product) == "" {
		return "", errors.New("structured message sender is incomplete")
	}
	from := message.From.Name
	if from == "" {
		from = message.From.UUID
	}
	attributes := []string{`from="` + safeAttribute(from) + `"`, `from-session="` + safeAttribute(message.From.UUID) + `"`}
	metadata, err := json.Marshal(struct {
		FromProduct string   `json:"fromProduct"`
		MessageID   string   `json:"messageId"`
		Groups      []string `json:"groups"`
	}{
		FromProduct: message.From.Product, MessageID: message.ID, Groups: message.From.Groups,
	})
	if err != nil {
		return "", err
	}
	return "<cross-session-message " + strings.Join(attributes, " ") + ">\n[codex-peer-metadata: " + string(metadata) + "]\n" +
		escapeEnvelopeBody(message.Body) + "\n</cross-session-message>", nil
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
		output.WriteString(message[:index])
		end := index + len(closing)
		output.WriteString("<\\/cross-session-message")
		message, lower = message[end:], lower[end:]
	}
}
