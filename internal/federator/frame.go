package federator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
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
