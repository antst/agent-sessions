package federator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	fromAttributeRE        = regexp.MustCompile(`(from=")[^"]*(")`)
	fromSessionAttributeRE = regexp.MustCompile(`(from-session=")[^"]*(")`)
	fromNameAttributeRE    = regexp.MustCompile(`(from-name=")[^"]*(")`)
)

func sourcePeerID(frame json.RawMessage, local map[string]localPeer) (string, error) {
	var value map[string]any
	if json.Unmarshal(frame, &value) != nil {
		return "", errors.New("peer frame is not a JSON object")
	}
	from, _ := value["from"].(string)
	path := decodeUDS(from)
	for id, peer := range local {
		if peer.Socket == path {
			return id, nil
		}
	}
	return "", errors.New("cannot map frame sender to a local live peer")
}

func rewriteInboundFrame(frame json.RawMessage, target localPeer, source Peer, sourceShadow string) (json.RawMessage, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	shadowAddress := encodeUDS(sourceShadow)
	value["from"] = shadowAddress
	if _, exists := value["session_id"]; exists {
		value["session_id"] = target.SessionID
	}
	if message, ok := value["message"].(map[string]any); ok {
		if content, ok := message["content"].(string); ok {
			content = replaceEnvelopeAttribute(content, fromAttributeRE, shadowAddress)
			content = replaceEnvelopeAttribute(content, fromSessionAttributeRE, source.GlobalID)
			content = replaceEnvelopeAttribute(content, fromNameAttributeRE, source.DisplayName)
			message["content"] = content
		}
	}
	return json.Marshal(value)
}

func replaceEnvelopeAttribute(content string, pattern *regexp.Regexp, replacement string) string {
	newline := strings.IndexByte(content, '\n')
	if newline < 0 || !strings.HasPrefix(content, "<cross-session-message") {
		return content
	}
	first := content[:newline]
	if !pattern.MatchString(first) {
		return content
	}
	escaped := strings.ReplaceAll(safeAttribute(replacement), "$", "$$")
	first = pattern.ReplaceAllString(first, "${1}"+escaped+"${2}")
	return first + content[newline:]
}

func encodeUDS(path string) string {
	var output strings.Builder
	output.WriteString("uds:")
	for _, value := range []byte(path) {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || strings.ContainsRune(":_/.\\-", rune(value)) {
			output.WriteByte(value)
		} else {
			fmt.Fprintf(&output, "%%%02X", value)
		}
	}
	return output.String()
}

func decodeUDS(address string) string {
	value := strings.TrimPrefix(address, "uds:")
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func safeAttribute(value string) string {
	return strings.NewReplacer("\"", "", "<", "", ">", "", "\n", "", "\r", "").Replace(value)
}
