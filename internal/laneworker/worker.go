package laneworker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

func TokenEndpoint(token string) (string, error) {
	body, err := base64.RawURLEncoding.DecodeString(token)
	var payload tokenPayload
	if err != nil || json.Unmarshal(body, &payload) != nil || !strings.HasPrefix(payload.Endpoint, "/") || len(payload.Nonce) != 64 {
		return "", errors.New("lane worker launch token is invalid")
	}
	return payload.Endpoint, nil
}
