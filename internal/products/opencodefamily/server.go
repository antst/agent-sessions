package opencodefamily

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

type ServerOpenRequest struct {
	Key             string
	Cwd             string
	Arguments       []string
	Env             []productruntime.EnvVar
	SensitiveEnv    []productruntime.SensitiveEnvVar
	PermissionRules []PermissionRule
}

type ServerRecoveryRequest struct {
	Key             string
	NativeSessionID string
	PriorGeneration uint64
	Arguments       []string
}

type ServerManager interface {
	Open(context.Context, ServerOpenRequest) (*LiveServer, error)
	Recover(context.Context, ServerRecoveryRequest) (*LiveServer, error)
}

type EndpointAllocator func(context.Context, string) (string, error)
type RecoveryDirectory func(context.Context, string, string) (string, error)
type CredentialSource func() (string, productruntime.SensitiveValue, error)

type OwnedServerManagerConfig struct {
	ProductID        string
	Dialect          Dialect
	Executable       string
	Supervisor       productruntime.OwnedProcessSupervisor
	AllocateEndpoint EndpointAllocator
	RecoveryCwd      RecoveryDirectory
	Credentials      CredentialSource
	Limits           productserver.Limits
}

type OwnedServerManager struct {
	config OwnedServerManagerConfig
}

func NewOwnedServerManager(config OwnedServerManagerConfig) (*OwnedServerManager, error) {
	if config.ProductID == "" || config.Executable == "" || config.Supervisor == nil ||
		config.Dialect != DialectOpenCode && config.Dialect != DialectKilo {
		return nil, productruntime.ErrProtocol
	}
	if config.AllocateEndpoint == nil {
		config.AllocateEndpoint = allocateLoopbackEndpoint
	}
	if config.Credentials == nil {
		config.Credentials = secureCredential
	}
	return &OwnedServerManager{config: config}, nil
}

func allocateLoopbackEndpoint(context.Context, string) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", productruntime.ErrUnavailable
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", productruntime.ErrCleanupDebt
	}
	return "http://" + address, nil
}

func secureCredential() (string, productruntime.SensitiveValue, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", productruntime.SensitiveValue{}, productruntime.ErrUnavailable
	}
	return "agent-sessions", productruntime.NewSensitiveValue(hex.EncodeToString(raw[:])), nil
}

func (manager *OwnedServerManager) Open(ctx context.Context, request ServerOpenRequest) (*LiveServer, error) {
	return manager.open(ctx, request.Key, request.Cwd, request.Arguments, request.Env, request.SensitiveEnv)
}

func (manager *OwnedServerManager) Recover(ctx context.Context, request ServerRecoveryRequest) (*LiveServer, error) {
	if manager.config.RecoveryCwd == nil || !validNativeID(request.NativeSessionID, "ses_") {
		return nil, productruntime.ErrUnsupportedRecovery
	}
	cwd, err := manager.config.RecoveryCwd(ctx, request.Key, request.NativeSessionID)
	if err != nil {
		return nil, fmt.Errorf("%w: recovery directory unavailable", productruntime.ErrUnsupportedRecovery)
	}
	return manager.open(ctx, request.Key, cwd, request.Arguments, nil, nil)
}

func (manager *OwnedServerManager) open(
	ctx context.Context,
	key, cwd string,
	arguments []string,
	environment []productruntime.EnvVar,
	sensitive []productruntime.SensitiveEnvVar,
) (*LiveServer, error) {
	if strings.TrimSpace(key) == "" || !validDirectory(cwd) || unsafeServerArguments(arguments) {
		return nil, productruntime.ErrProtocol
	}
	endpoint, err := manager.config.AllocateEndpoint(ctx, key)
	if err != nil {
		return nil, err
	}
	host, port, err := endpointParts(endpoint)
	if err != nil {
		return nil, err
	}
	username, password, err := manager.config.Credentials()
	if err != nil || username == "" || password.Empty() {
		return nil, productruntime.ErrUnavailable
	}
	auth, err := productserver.NewBasicAuth(username, password)
	if err != nil {
		return nil, productruntime.ErrProtocol
	}
	command := productruntime.NativeCommand{
		Path:         manager.config.Executable,
		Args:         append([]string{"serve", "--hostname", host, "--port", port}, arguments...),
		Env:          append([]productruntime.EnvVar(nil), environment...),
		SensitiveEnv: append([]productruntime.SensitiveEnvVar(nil), sensitive...),
		Cwd:          cwd,
	}
	prefix := "OPENCODE"
	if manager.config.Dialect == DialectKilo {
		prefix = "KILO"
	}
	command.Env = append(command.Env, productruntime.EnvVar{Name: prefix + "_SERVER_USERNAME", Value: username})
	command.SensitiveEnv = append(command.SensitiveEnv, productruntime.SensitiveEnvVar{Name: prefix + "_SERVER_PASSWORD", Value: password})
	var typed *Client
	owned, err := productserver.StartOwnedServer(ctx, productserver.OwnedServerConfig{
		Command: command, Endpoint: endpoint, Auth: auth, Limits: manager.config.Limits,
		Supervisor: manager.config.Supervisor,
		Ready: func(readyCtx context.Context, raw *productserver.Client) error {
			candidate, typedErr := NewClient(ClientConfig{HTTP: raw, Directory: cwd, Dialect: manager.config.Dialect})
			if typedErr != nil {
				return typedErr
			}
			features, probeErr := candidate.ProbeDocument(readyCtx, []string{"/session", "/event"})
			if probeErr != nil || !features["/session"] || !features["/event"] {
				return productruntime.ErrIncompatible
			}
			typed = candidate
			return nil
		},
	})
	if err != nil {
		if owned != nil && errors.Is(err, productserver.ErrCleanupDebt) {
			return &LiveServer{owned: owned, client: typed}, fmt.Errorf("%w: native server startup cleanup", productruntime.ErrCleanupDebt)
		}
		return nil, mapTransportError(err)
	}
	if typed == nil {
		_ = owned.Close(context.Background())
		return nil, productruntime.ErrProtocol
	}
	return &LiveServer{owned: owned, client: typed, endpoint: endpoint, username: username, password: password}, nil
}

func unsafeServerArguments(arguments []string) bool {
	reserved := map[string]bool{
		"serve": true, "attach": true, "daemon": true, "--mini": true,
		"--hostname": true, "--port": true,
	}
	for _, argument := range arguments {
		key := argument
		if index := strings.IndexByte(key, '='); index >= 0 {
			key = key[:index]
		}
		if reserved[key] || strings.ContainsRune(argument, '\x00') {
			return true
		}
	}
	return false
}

func endpointParts(endpoint string) (string, string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Path != "" || parsed.RawQuery != "" || parsed.User != nil {
		return "", "", productruntime.ErrProtocol
	}
	ip := net.ParseIP(parsed.Hostname())
	port := parsed.Port()
	portNumber, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", productruntime.ErrProtocol
	}
	return ip.String(), port, nil
}

type LiveServer struct {
	mu       sync.Mutex
	owned    *productserver.OwnedServer
	client   *Client
	closeFn  func(context.Context) error
	endpoint string
	username string
	password productruntime.SensitiveValue
	closed   bool
}

func (*LiveServer) MarshalJSON() ([]byte, error) {
	return nil, errors.New("live product server is transient and cannot be serialized")
}

func (server *LiveServer) Client() *Client {
	if server == nil {
		return nil
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed {
		return nil
	}
	return server.client
}

func (server *LiveServer) ProcessRef() productruntime.OwnedProcessRef {
	if server == nil || server.owned == nil {
		return productruntime.OwnedProcessRef{}
	}
	return server.owned.Ref()
}

func (server *LiveServer) Close(ctx context.Context) error {
	if server == nil {
		return nil
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed {
		return nil
	}
	owned := server.owned
	closeFn := server.closeFn
	if closeFn != nil {
		if err := closeFn(ctx); err != nil {
			return err
		}
	} else if owned == nil {
		return productruntime.ErrCleanupDebt
	} else if err := owned.Close(ctx); err != nil {
		return fmt.Errorf("%w: exact server exit unproven", productruntime.ErrCleanupDebt)
	}
	server.closed = true
	server.password = productruntime.SensitiveValue{}
	server.endpoint, server.username = "", ""
	server.client = nil
	return nil
}

// BuildKiloAttach constructs the full isolated attach TUI. --mini and every
// endpoint/topology override fail closed before the native process is invoked.
func (server *LiveServer) BuildKiloAttach(cwd, nativeSessionID string, arguments []string, environment []productruntime.EnvVar) (productruntime.NativeCommand, error) {
	if server == nil || !validDirectory(cwd) || !validNativeID(nativeSessionID, "ses_") || unsafeAttachArguments(arguments) {
		return productruntime.NativeCommand{}, productruntime.ErrUnsupportedPolicy
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed || server.endpoint == "" || server.username == "" || server.password.Empty() {
		return productruntime.NativeCommand{}, productruntime.ErrStale
	}
	args := []string{"attach", server.endpoint, "--dir", cwd, "--session", nativeSessionID}
	args = append(args, arguments...)
	return productruntime.NativeCommand{
		Path: "kilo", Args: args, Cwd: cwd,
		Env:          append(append([]productruntime.EnvVar(nil), environment...), productruntime.EnvVar{Name: "KILO_SERVER_USERNAME", Value: server.username}),
		SensitiveEnv: []productruntime.SensitiveEnvVar{{Name: "KILO_SERVER_PASSWORD", Value: server.password}},
	}, nil
}

func unsafeAttachArguments(arguments []string) bool {
	for _, argument := range arguments {
		key := argument
		if index := strings.IndexByte(key, '='); index >= 0 {
			key = key[:index]
		}
		switch key {
		case "--mini", "--session", "-s", "--continue", "-c", "--fork", "--cloud-fork",
			"--dir", "--hostname", "--port", "--password", "-p", "--username", "-u",
			"--replay", "--no-replay", "--replay-limit", "attach", "serve", "daemon":
			return true
		}
		if strings.ContainsRune(argument, '\x00') {
			return true
		}
	}
	return false
}

func executableBase(value string) string { return filepath.Base(strings.TrimSpace(value)) }
