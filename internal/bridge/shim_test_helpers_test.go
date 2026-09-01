package bridge

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"testing"
)

// serveTestMutableShim implements the lifecycle-control subset used by tests
// that deliberately provide a socket fixture instead of starting a real shim.
// Keeping the acknowledgement behavior here prevents those fixtures from
// silently drifting behind the production shim protocol.
func serveTestMutableShim(t *testing.T, listener net.Listener, statePath string) {
	t.Helper()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestMutableShimConnection(connection, statePath)
		}
	}()
}

func serveTestMutableShimConnection(connection net.Conn, statePath string) {
	defer func() { _ = connection.Close() }()
	scanner := bufio.NewScanner(connection)
	for scanner.Scan() {
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || stringValue(frame["type"]) != "control" {
			continue
		}
		if stringValue(frame["action"]) == "inspect" {
			state := readJSONMap(statePath)
			if state == nil {
				state = map[string]any{}
			}
			state["type"] = "peer_inspection"
			body, _ := json.Marshal(state)
			_, _ = connection.Write(append(body, '\n'))
			continue
		}
		state := readJSONMap(statePath)
		if state == nil {
			state = map[string]any{}
		}
		switch stringValue(frame["action"]) {
		case "update":
			for _, field := range []string{"cwd", "name", "nameSource", "permissionMode", "status", "supervisorSocket"} {
				if value := stringValue(frame[field]); value != "" {
					state[field] = value
				}
			}
		case "status":
			if value := stringValue(frame["status"]); value != "" {
				state["status"] = value
			}
		case "permission_mode":
			if value := stringValue(frame["permissionMode"]); value != "" {
				state["permissionMode"] = value
			}
		case "rename":
			if value := stringValue(frame["name"]); value != "" {
				state["name"] = sanitizeName(value)
				state["nameSource"] = "manual"
			}
		case "shutdown":
			_ = os.Remove(statePath)
			return
		}
		_ = writeJSONAtomic(statePath, state)
	}
}
