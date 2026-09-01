package sessiontools

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// ProductLabel resolves only an exact catalog product; empty and unknown
// entrypoints never masquerade as Claude or Codex.
func ProductLabel(product string) (string, error) {
	descriptor, ok := productcatalog.ByID(product)
	if !ok {
		return "", fmt.Errorf("session tool product %q is unsupported", product)
	}
	return descriptor.Label, nil
}

// WrapPeerMessage preserves the native cross-session carrier while deriving
// product identity from the sole catalog and escaping closing-tag injection.
func WrapPeerMessage(product, from, sessionID, name, mode, messageID, sentAt, message string) (string, error) {
	if _, err := ProductLabel(product); err != nil {
		return "", err
	}
	attributes := []string{}
	if from != "" {
		attributes = append(attributes, `from="`+safeAttribute(from)+`"`)
	}
	if normalizedSessionID, ok := normalizeNativeID(sessionID); ok {
		attributes = append(attributes, `from-session="`+normalizedSessionID+`"`)
	}
	if name != "" {
		attributes = append(attributes, `from-name="`+safeAttribute(name)+`"`)
	}
	if mode == "bypass" || mode == "prompting" {
		attributes = append(attributes, `from-mode="`+mode+`"`)
	}
	metadata, err := json.Marshal(map[string]any{"fromProduct": product, "messageId": safeAttribute(messageID), "sentAt": safeAttribute(sentAt)})
	if err != nil {
		return "", err
	}
	suffix := ""
	if len(attributes) > 0 {
		suffix = " " + strings.Join(attributes, " ")
	}
	return "<cross-session-message" + suffix + ">\n[codex-peer-metadata: " + string(metadata) + "]\n" + escapeEnvelopeBody(message) + "\n</cross-session-message>", nil
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
