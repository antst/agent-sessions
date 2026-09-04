package laneworker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/sessionidentity"
)

type WorkerConfig struct {
	Product        string
	Capabilities   productruntime.LaneCapabilitySet
	ExtraArguments []productruntime.LaneExtraArgument
	Readiness      func(context.Context) productruntime.LaneReadiness
	Body           productruntime.LaneWorkerBody
	Schema         *productruntime.LaneWireSchema
	Dial           func(context.Context, string, string) (net.Conn, error)
}

type Worker struct {
	config  WorkerConfig
	ctx     context.Context
	cancel  context.CancelFunc
	conn    *livepresence.Connection
	session *productruntime.LaneWorkerSession
	seq     atomic.Int64
	once    sync.Once
}

func Run(ctx context.Context, config WorkerConfig) error {
	token := os.Getenv(EnvLaunchToken)
	if token == "" {
		return errors.New("lane worker launch token is missing")
	}
	_ = os.Unsetenv(EnvLaunchToken)
	return run(ctx, token, config)
}

func run(ctx context.Context, token string, config WorkerConfig) error {
	endpoint, err := TokenEndpoint(token)
	if err != nil || productcatalog.ValidateToken(config.Product) != nil ||
		config.Body == nil || config.Readiness == nil || config.Schema == nil {
		return errors.New("lane worker configuration is incomplete")
	}
	if config.Dial == nil {
		config.Dial = (&net.Dialer{}).DialContext
	}
	runCtx, cancel := context.WithCancel(ctx)
	socket, err := config.Dial(runCtx, "unix", endpoint)
	if err != nil {
		cancel()
		return err
	}
	worker := &Worker{
		config: config,
		ctx:    runCtx,
		cancel: cancel,
		conn:   livepresence.NewConnection(socket),
	}
	worker.session = productruntime.NewLaneWorkerSession(
		runCtx, config.Body, config.Capabilities, worker.update, worker.close,
	)
	if err := worker.hello(token); err != nil {
		worker.close()
		return err
	}
	err = worker.serve()
	worker.close()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func TokenEndpoint(token string) (string, error) {
	body, err := base64.RawURLEncoding.DecodeString(token)
	var payload tokenPayload
	if err != nil || json.Unmarshal(body, &payload) != nil ||
		!strings.HasPrefix(payload.Endpoint, "/") || len(payload.Nonce) != 64 {
		return "", errors.New("lane worker launch token is invalid")
	}
	return payload.Endpoint, nil
}

func (w *Worker) hello(token string) error {
	hello := productruntime.LaneWorkerHello{
		Protocol:       1,
		LaunchToken:    token,
		Product:        w.config.Product,
		Capabilities:   w.config.Capabilities,
		ExtraArguments: append([]productruntime.LaneExtraArgument(nil), w.config.ExtraArguments...),
		Readiness:      w.config.Readiness(w.ctx),
	}
	params, _ := json.Marshal(hello)
	request := livepresence.Frame{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "lane.worker.hello",
		Params:  params,
	}
	if err := w.conn.Write(request); err != nil {
		return err
	}
	var response livepresence.Frame
	if err := w.conn.DecodeWire(&response); err != nil ||
		!livepresence.ValidFrame(response) || string(response.ID) != "1" || !empty(response.Result) {
		return errors.New("lane worker hello was not acknowledged")
	}
	w.seq.Store(1)
	return nil
}

func (w *Worker) serve() error {
	for {
		var frame livepresence.Frame
		if err := w.conn.DecodeWire(&frame); err != nil {
			return err
		}
		if !livepresence.ValidFrame(frame) || !integerID(frame.ID) {
			return errors.New("lane worker received an invalid private frame")
		}
		if frame.Method == "" {
			if !w.conn.Resolve(frame) {
				return errors.New("lane worker received an unknown response")
			}
			continue
		}
		go w.handle(frame)
	}
}

func (w *Worker) handle(frame livepresence.Frame) {
	result, after, err := w.dispatch(frame.Method, frame.Params)
	if err != nil {
		_ = w.conn.Write(livepresence.FailureFromError(frame.ID, frame.Method, err))
		return
	}
	body, _ := json.Marshal(result)
	if err := w.conn.Write(livepresence.Success(frame.ID, body)); err != nil {
		w.close()
		return
	}
	if after != nil {
		_ = after()
	}
}

func (w *Worker) dispatch(method string, raw json.RawMessage) (any, func() error, error) {
	switch method {
	case "lane.session.open":
		var request productruntime.LaneOpenRequest
		if err := w.config.Schema.Decode("LaneOpenRequest", raw, &request); err != nil {
			return nil, nil, livepresence.ErrInvalidParams
		}
		result, err := w.session.Open(request)
		if err == nil && !sessionidentity.ValidNativeID(result.NativeID) {
			err = productruntime.ErrProtocol
		}
		return result, nil, err
	case "lane.turn.start":
		var request productruntime.LaneTurnStartRequest
		if err := w.config.Schema.Decode("LaneTurnStartRequest", raw, &request); err != nil {
			return nil, nil, livepresence.ErrInvalidParams
		}
		result, err := w.session.Start(request)
		return result, nil, err
	case "lane.turn.wait":
		messageID, ok := nativeMessageID(raw)
		if !ok {
			return nil, nil, livepresence.ErrInvalidParams
		}
		result, after, err := w.session.Wait(w.ctx, messageID)
		return result, after, err
	case "lane.turn.interrupt":
		if !empty(raw) {
			return nil, nil, livepresence.ErrInvalidParams
		}
		return struct{}{}, nil, w.session.Interrupt()
	case "lane.session.archive":
		if !empty(raw) {
			return nil, nil, livepresence.ErrInvalidParams
		}
		after, err := w.session.Archive()
		return struct{}{}, after, err
	default:
		return nil, nil, productruntime.ErrUnauthorized
	}
}

func (w *Worker) update(projection productruntime.LaneStatusProjection) error {
	_, err := w.conn.CallWire(
		w.ctx,
		w.seq.Add(1),
		"session.update",
		projection,
		func(raw json.RawMessage) bool { return empty(raw) },
	)
	return err
}

func (w *Worker) close() {
	w.once.Do(func() {
		w.cancel()
		_ = w.conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = w.session.Close(ctx)
	})
}

func nativeMessageID(raw json.RawMessage) (string, bool) {
	spec, _ := livepresence.LookupMethod("lane.turn.wait")
	if !livepresence.ValidMethodParams(spec, raw) {
		return "", false
	}
	var request struct {
		NativeMessageID string `json:"native_message_id"`
	}
	return request.NativeMessageID, productruntime.DecodeClosed(raw, &request) == nil
}

func empty(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte(`{}`))
}
