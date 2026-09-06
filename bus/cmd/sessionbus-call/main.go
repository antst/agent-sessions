package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	sdk "github.com/antst/sessionbus/bus/sdk/go"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(arguments []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("sessionbus-call", flag.ContinueOnError)
	set.SetOutput(stderr)
	name, groups, socket := "sessionbus-call", "", os.Getenv("SESSIONBUS_SOCKET")
	set.StringVar(&name, "name", name, "peer name")
	set.StringVar(&groups, "g", groups, "comma-separated groups")
	set.StringVar(&socket, "socket", socket, "sessionbus unix socket")
	if set.Parse(arguments) != nil || set.NArg() < 1 || set.NArg() > 2 {
		return 2
	}
	params := json.RawMessage(`{}`)
	if set.NArg() == 2 {
		params = json.RawMessage(set.Arg(1))
	}
	if !json.Valid(params) {
		fmt.Fprintln(stderr, "params-json is not valid JSON")
		return 2
	}
	if err := os.Setenv("SESSIONBUS_SOCKET", socket); err != nil {
		writeError(stdout, err)
		return 1
	}
	id, err := newID()
	if err != nil {
		writeError(stdout, err)
		return 1
	}
	identity := sdk.PeerIdentity{Product: "sessionbus-call", SessionID: id, Name: name, Groups: splitGroups(groups), Info: map[string]any{}}
	var outputMu sync.Mutex
	peer, err := sdk.ConnectPeer(identity, func(_ context.Context, _ sdk.PeerIdentity, request sdk.DeliveryRequest) (sdk.DeliveryReceipt, error) {
		outputMu.Lock()
		defer outputMu.Unlock()
		_ = json.NewEncoder(stderr).Encode(request)
		return sdk.DeliveryReceipt{Disposition: "injected"}, nil
	})
	if err == nil {
		<-peer.Ready()
		defer peer.Shutdown()
		var result json.RawMessage
		result, err = peer.Call(context.Background(), set.Arg(0), params)
		if err == nil {
			fmt.Fprintln(stdout, string(result))
			return 0
		}
	}
	writeError(stdout, err)
	return 1
}

func splitGroups(value string) []string {
	if value == "" {
		return []string{}
	}
	return strings.Split(value, ",")
}

func newID() (string, error) {
	value := make([]byte, 16)
	_, err := rand.Read(value)
	return hex.EncodeToString(value), err
}

func writeError(output io.Writer, err error) {
	var protocolError *sdk.ProtocolError
	if errors.As(err, &protocolError) {
		_ = json.NewEncoder(output).Encode(protocolError)
		return
	}
	_ = json.NewEncoder(output).Encode(map[string]string{"error": err.Error()})
}
