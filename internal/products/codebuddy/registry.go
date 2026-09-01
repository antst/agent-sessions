package codebuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productruntime"
)

const (
	maxRegistryRows       = 256
	maxRegistryRowBytes   = 64 << 10
	maxRegistryTotalBytes = 4 << 20
)

type RegistryIdentity struct {
	Path   string
	Device uint64
	Inode  uint64
	Bytes  int64
	Digest [sha256.Size]byte
}

type WorkerClaim struct {
	SessionID string
	PID       int
	Kind      string
	Cwd       string
	Name      string
	Endpoint  string
	Registry  RegistryIdentity
}

type WorkerRegistry interface {
	FindInteractive(context.Context, string) (WorkerClaim, error)
	VerifyUnchanged(context.Context, WorkerClaim) error
}

// FileWorkerRegistry reads CodeBuddy's supported ~/.codebuddy/sessions worker
// registry. Registry URLs remain untrusted claims until the peer verifier has
// correlated socket and process ownership.
type FileWorkerRegistry struct{ root string }

func NewFileWorkerRegistry(root string) (*FileWorkerRegistry, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("%w: registry root must be canonical and absolute", ErrInvalidRegistry)
	}
	return &FileWorkerRegistry{root: root}, nil
}

func (registry *FileWorkerRegistry) FindInteractive(ctx context.Context, sessionID string) (WorkerClaim, error) {
	if registry == nil || ctx == nil || !validNativeID(sessionID) {
		return WorkerClaim{}, ErrInvalidRegistry
	}
	rootInfo, err := os.Lstat(registry.root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return WorkerClaim{}, errors.Join(ErrInvalidRegistry, err)
	}
	directory, err := os.Open(registry.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WorkerClaim{}, errors.Join(ErrWorkerNotFound, err)
		}
		return WorkerClaim{}, errors.Join(ErrInvalidRegistry, err)
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() || !os.SameFile(rootInfo, info) {
		return WorkerClaim{}, ErrInvalidRegistry
	}
	names, err := directory.Readdirnames(maxRegistryRows + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return WorkerClaim{}, errors.Join(ErrInvalidRegistry, err)
	}
	if len(names) > maxRegistryRows {
		return WorkerClaim{}, fmt.Errorf("%w: registry row bound exceeded", ErrInvalidRegistry)
	}
	sort.Strings(names)
	var matches []WorkerClaim
	var total int64
	for _, name := range names {
		select {
		case <-ctx.Done():
			return WorkerClaim{}, ctx.Err()
		default:
		}
		if filepath.Ext(name) != ".json" || filepath.Base(name) != name {
			continue
		}
		claim, rawBytes, err := registry.readRowAt(directory, name, sessionID)
		if err != nil {
			return WorkerClaim{}, err
		}
		total += rawBytes
		if total > maxRegistryTotalBytes {
			return WorkerClaim{}, fmt.Errorf("%w: registry byte bound exceeded", ErrInvalidRegistry)
		}
		if claim.SessionID == sessionID && claim.Kind == "interactive" {
			matches = append(matches, claim)
		}
	}
	if len(matches) == 0 {
		return WorkerClaim{}, ErrWorkerNotFound
	}
	if len(matches) != 1 {
		return WorkerClaim{}, ErrWorkerAmbiguous
	}
	return matches[0], nil
}

func (registry *FileWorkerRegistry) VerifyUnchanged(ctx context.Context, expected WorkerClaim) error {
	if registry == nil || ctx == nil || expected.Registry.Path == "" || filepath.Dir(expected.Registry.Path) != registry.root {
		return ErrInvalidRegistry
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	directory, err := registry.openRoot()
	if err != nil {
		return err
	}
	defer directory.Close()
	current, _, err := registry.readRowAt(directory, filepath.Base(expected.Registry.Path), expected.SessionID)
	if err != nil {
		return errors.Join(ErrInvalidRegistry, err)
	}
	if current.SessionID != expected.SessionID || current.PID != expected.PID || current.Kind != expected.Kind ||
		current.Cwd != expected.Cwd || current.Endpoint != expected.Endpoint || current.Registry != expected.Registry {
		return fmt.Errorf("%w: worker row changed during attestation", productruntime.ErrStale)
	}
	return nil
}

func (registry *FileWorkerRegistry) readRowAt(directory *os.File, name, wantedSession string) (WorkerClaim, int64, error) {
	body, fileInfo, err := readNoFollowAt(directory, name, maxRegistryRowBytes)
	return registry.parseRow(filepath.Join(registry.root, name), wantedSession, body, fileInfo, err)
}

func (registry *FileWorkerRegistry) parseRow(path, wantedSession string, body []byte, fileInfo os.FileInfo, err error) (WorkerClaim, int64, error) {
	if err != nil {
		return WorkerClaim{}, 0, errors.Join(ErrInvalidRegistry, err)
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Size() != int64(len(body)) || len(body) == 0 {
		return WorkerClaim{}, 0, ErrInvalidRegistry
	}
	device, inode, ok := fileIdentity(fileInfo)
	if !ok || device == 0 || inode == 0 {
		return WorkerClaim{}, 0, ErrInvalidRegistry
	}
	identity := RegistryIdentity{Path: path, Device: device, Inode: inode, Bytes: int64(len(body)), Digest: sha256.Sum256(body)}
	var selector struct {
		SessionID string `json:"sessionId"`
		Kind      string `json:"kind"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&selector); err != nil {
		return WorkerClaim{}, 0, errors.Join(ErrInvalidRegistry, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return WorkerClaim{}, 0, ErrInvalidRegistry
	}
	selector.SessionID = strings.TrimSpace(selector.SessionID)
	selector.Kind = strings.TrimSpace(selector.Kind)
	if selector.SessionID != wantedSession || selector.Kind != "interactive" {
		return WorkerClaim{SessionID: selector.SessionID, Kind: selector.Kind, Registry: identity}, int64(len(body)), nil
	}
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		return WorkerClaim{}, 0, errors.Join(ErrInvalidRegistry, err)
	}
	if containsCredentialField(raw) {
		return WorkerClaim{}, 0, fmt.Errorf("%w: selected interactive registry row contains a credential field", ErrInvalidRegistry)
	}
	pid, err := jsonPositiveInt(raw["pid"])
	if err != nil {
		return WorkerClaim{}, 0, ErrInvalidRegistry
	}
	endpoint, _ := raw["endpoint"].(string)
	if endpoint == "" {
		endpoint, _ = raw["url"].(string)
	}
	endpoint, err = canonicalLoopbackEndpoint(endpoint)
	if err != nil {
		return WorkerClaim{}, 0, errors.Join(ErrInvalidRegistry, err)
	}
	claim := WorkerClaim{
		SessionID: stringField(raw, "sessionId"), PID: pid, Kind: stringField(raw, "kind"),
		Cwd: filepath.Clean(stringField(raw, "cwd")), Name: stringField(raw, "name"), Endpoint: endpoint,
		Registry: identity,
	}
	if !validNativeID(claim.SessionID) || claim.Kind != "interactive" || !filepath.IsAbs(claim.Cwd) || !utf8.ValidString(claim.Name) {
		return WorkerClaim{}, 0, ErrInvalidRegistry
	}
	return claim, int64(len(body)), nil
}

func (registry *FileWorkerRegistry) openRoot() (*os.File, error) {
	rootInfo, err := os.Lstat(registry.root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.Join(ErrInvalidRegistry, err)
	}
	directory, err := os.Open(registry.root)
	if err != nil {
		return nil, errors.Join(ErrInvalidRegistry, err)
	}
	info, err := directory.Stat()
	if err != nil || !info.IsDir() || !os.SameFile(rootInfo, info) {
		directory.Close()
		return nil, ErrInvalidRegistry
	}
	return directory, nil
}

func canonicalLoopbackEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", ErrInvalidRegistry
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() || parsed.Port() == "" {
		return "", ErrInvalidRegistry
	}
	parsed.Host = net.JoinHostPort(ip.String(), parsed.Port())
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String(), nil
}

func jsonPositiveInt(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, ErrInvalidRegistry
	}
	integer, err := number.Int64()
	if err != nil || integer <= 1 || integer > int64(^uint(0)>>1) {
		return 0, ErrInvalidRegistry
	}
	return int(integer), nil
}

func stringField(object map[string]any, name string) string {
	value, _ := object[name].(string)
	return strings.TrimSpace(value)
}

func containsCredentialField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "_", ""), "-", ""))
			for _, forbidden := range []string{"password", "secret", "token", "authorization", "credential"} {
				if strings.Contains(normalized, forbidden) {
					return true
				}
			}
			if normalized == "auth" || strings.HasSuffix(normalized, "auth") {
				return true
			}
			if containsCredentialField(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsCredentialField(child) {
				return true
			}
		}
	}
	return false
}

func digestPrefix(digest [sha256.Size]byte) string {
	return hex.EncodeToString(digest[:16])
}
