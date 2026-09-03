package sessiontools

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productruntime"
)

// RenderNativeMessage is the one Go-side rendering of a structured v1
// delivery at a product's native text-input boundary.
func RenderNativeMessage(message productruntime.NativeMessage) (string, error) {
	if strings.TrimSpace(message.From.UUID) == "" || strings.TrimSpace(message.From.Product) == "" {
		return "", errors.New("structured message sender is incomplete")
	}
	attributes := []string{`from="` + safeAttribute(message.From.Name) + `"`, `from-session="` + safeAttribute(message.From.UUID) + `"`}
	metadata, err := json.Marshal(map[string]any{
		"fromProduct": message.From.Product, "messageId": safeAttribute(message.ID), "groups": message.From.Groups,
	})
	if err != nil {
		return "", err
	}
	return "<cross-session-message " + strings.Join(attributes, " ") + ">\n[codex-peer-metadata: " + string(metadata) + "]\n" +
		escapeEnvelopeBody(message.Body) + "\n</cross-session-message>", nil
}

func safeAttribute(value string) string {
	value = strings.NewReplacer(`"`, "", "<", "", ">", "", "\n", "", "\r", "").Replace(value)
	if len(value) <= 200 {
		return value
	}
	value = value[:200]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
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
		if end < len(message) {
			next := message[end]
			if next != '>' && next != '/' && next != ' ' && next != '\t' && next != '\n' && next != '\r' {
				output.WriteString(message[:end])
				message, lower = message[end:], lower[end:]
				continue
			}
		}
		output.WriteString(message[:index])
		output.WriteString("<\\/")
		output.WriteString(message[index+2 : end])
		message, lower = message[end:], lower[end:]
	}
}
