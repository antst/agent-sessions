package laneworker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antst/agent-sessions/internal/livepresence"
	"github.com/antst/agent-sessions/internal/productruntime"
)

const EnvLaunchToken = "AGENT_SESSIONS_LAUNCH_TOKEN"

type TokenPurpose string

const (
	PurposeLaunch TokenPurpose = "launch"
	PurposeDoctor TokenPurpose = "doctor"
)

type tokenPayload struct {
	Endpoint string `json:"endpoint"`
	Nonce    string `json:"nonce"`
}

type Registration struct {
	token      string
	purpose    TokenPurpose
	product    string
	laneID     string
	generation uint64
	expires    time.Time
	ready      chan *Binding
}

type Authority struct {
	endpoint string
	schema   *productruntime.LaneWireSchema
	now      func() time.Time
	mu       sync.Mutex
	tokens   map[string]*Registration
}

func NewAuthority(endpoint string, schema *productruntime.LaneWireSchema) (*Authority, error) {
	if !strings.HasPrefix(endpoint, "/") || schema == nil {
		return nil, errors.New("lane worker authority is incomplete")
	}
	return &Authority{endpoint: endpoint, schema: schema, now: time.Now, tokens: map[string]*Registration{}}, nil
}

func (a *Authority) Register(purpose TokenPurpose, product, laneID string, generation uint64, lifetime time.Duration) (*Registration, error) {
	if purpose != PurposeLaunch && purpose != PurposeDoctor || product == "" || generation == 0 || lifetime <= 0 || purpose == PurposeLaunch && laneID == "" || purpose == PurposeDoctor && laneID != "" {
		return nil, errors.New("lane worker token reservation is invalid")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(tokenPayload{Endpoint: a.endpoint, Nonce: hex.EncodeToString(nonce)})
	registration := &Registration{
		token: base64.RawURLEncoding.EncodeToString(payload), purpose: purpose, product: product, laneID: laneID,
		generation: generation, expires: a.now().Add(lifetime), ready: make(chan *Binding, 1),
	}
	a.mu.Lock()
	a.tokens[registration.token] = registration
	a.mu.Unlock()
	return registration, nil
}

func (a *Authority) Cancel(registration *Registration) {
	if registration == nil {
		return
	}
	a.mu.Lock()
	if a.tokens[registration.token] == registration {
		delete(a.tokens, registration.token)
	}
	a.mu.Unlock()
}

func (r *Registration) Token() productruntime.SensitiveValue {
	if r == nil {
		return productruntime.NewSensitiveValue("")
	}
	return productruntime.NewSensitiveValue(r.token)
}

func (r *Registration) Wait(ctx context.Context) (*Binding, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("lane worker wait is incomplete")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case binding := <-r.ready:
		return binding, nil
	}
}

func TokenEndpoint(token string) (string, error) {
	body, err := base64.RawURLEncoding.DecodeString(token)
	var payload tokenPayload
	if err != nil || json.Unmarshal(body, &payload) != nil || !strings.HasPrefix(payload.Endpoint, "/") || len(payload.Nonce) != 64 {
		return "", errors.New("lane worker launch token is invalid")
	}
	return payload.Endpoint, nil
}

type Binding struct {
	connection   *livepresence.Connection
	Hello        productruntime.LaneWorkerHello
	Purpose      TokenPurpose
	LaneID       string
	Generation   uint64
	sequence     atomic.Int64
	done         chan struct{}
	disconnected sync.Once
}

func (a *Authority) Accept(connection *livepresence.Connection, first livepresence.Frame) (*Binding, error) {
	if connection == nil || !livepresence.ValidRequest(first) || first.Method != "lane.worker.hello" || !integerID(first.ID) {
		return nil, errors.New("lane worker first frame is invalid")
	}
	var hello productruntime.LaneWorkerHello
	if err := a.schema.Decode("LaneWorkerHello", first.Params, &hello); err != nil {
		return nil, err
	}
	a.mu.Lock()
	registration := a.tokens[hello.LaunchToken]
	if registration == nil || !a.now().Before(registration.expires) || registration.product != hello.Product {
		a.mu.Unlock()
		return nil, errors.New("lane worker launch token was rejected")
	}
	delete(a.tokens, registration.token)
	a.mu.Unlock()
	binding := &Binding{connection: connection, Hello: hello, Purpose: registration.purpose, LaneID: registration.laneID, Generation: registration.generation, done: make(chan struct{})}
	if err := binding.connection.Write(livepresence.Success(first.ID, json.RawMessage(`{}`))); err != nil {
		binding.disconnect()
		return nil, err
	}
	registration.ready <- binding
	return binding, nil
}

func (b *Binding) Call(ctx context.Context, method string, params any, result any, validate func(json.RawMessage) bool) error {
	raw, err := b.connection.CallWire(ctx, b.sequence.Add(1), method, params, validate)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return productruntime.DecodeClosed(raw, result)
}

func (b *Binding) Serve(ctx context.Context, update func(productruntime.LaneStatusProjection) error) error {
	defer b.disconnect()
	stop := context.AfterFunc(ctx, func() { _ = b.connection.Close() })
	defer stop()
	for {
		var frame livepresence.Frame
		if err := b.connection.DecodeWire(&frame); err != nil {
			return err
		}
		if frame.Method == "" {
			if !b.connection.Resolve(frame) {
				return errors.New("lane worker returned an unknown response")
			}
			continue
		}
		response := livepresence.Failure(frame.ID, livepresence.NotPermitted, "Operation not permitted", map[string]any{"method": frame.Method})
		if frame.Method == "session.update" && update != nil {
			var projection productruntime.LaneStatusProjection
			if err := productruntime.DecodeClosed(frame.Params, &projection); err == nil && projection.Valid() {
				if err = update(projection); err == nil {
					response = livepresence.Success(frame.ID, json.RawMessage(`{}`))
				} else {
					response = livepresence.FailureFromError(frame.ID, frame.Method, err)
				}
			} else {
				response = livepresence.Failure(frame.ID, livepresence.InvalidParams, "Invalid params", map[string]any{"method": frame.Method})
			}
		}
		if err := b.connection.Write(response); err != nil {
			return err
		}
	}
}

func (b *Binding) Close() error          { return b.connection.Close() }
func (b *Binding) Done() <-chan struct{} { return b.done }

func (b *Binding) disconnect() {
	b.disconnected.Do(func() {
		b.connection.Fail()
		_ = b.connection.Close()
		close(b.done)
	})
}

func integerID(raw json.RawMessage) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	number, ok := value.(json.Number)
	parsed, err := number.Float64()
	return ok && err == nil && math.Trunc(parsed) == parsed && math.Abs(parsed) <= 9007199254740991
}

func Failure(first livepresence.Frame, err error) livepresence.Frame {
	return livepresence.FailureFromError(first.ID, first.Method, fmt.Errorf("%w: %v", productruntime.ErrProtocol, err))
}
