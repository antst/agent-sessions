package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/releaseinstall"
)

// ConnectorRequest identifies one authoritative product connector payload.
type ConnectorRequest struct {
	Product    string
	SourceRoot string
	Descriptor productcatalog.ConnectorDescriptor
}

// ConnectorMutation is one prepared, recoverable native connector change.
type ConnectorMutation interface {
	// Commit applies the prepared native mutation.
	Commit(context.Context) error
	// Rollback restores the exact captured prior native connector state.
	Rollback(context.Context) error
}

// ConnectorDriver performs supported product-native prepare and removal
// operations without inspecting credential values.
type ConnectorDriver interface {
	// Prepare validates and captures one reversible connector mutation.
	Prepare(context.Context, ConnectorRequest) (ConnectorMutation, error)
	// Remove invokes only the product-supported connector removal path.
	Remove(context.Context) error
}

// ConnectorCommandRunner is the narrow product-native command boundary used
// by connector transactions. Output is consumed only as bounded connector
// inventory metadata; credential and transcript stores are never opened.
type ConnectorCommandRunner interface {
	// LookPath resolves one selected native executable without invoking it.
	LookPath(string) (string, error)
	// Output returns bounded non-secret native connector inventory metadata.
	Output(context.Context, string, ...string) ([]byte, error)
	// Run invokes one structured product-native connector mutation.
	Run(context.Context, string, ...string) error
}

// NativeConnectorOptions selects native executables and the one supported
// Grok user-plugin payload root. Empty executable names use vendor defaults.
type NativeConnectorOptions struct {
	Runner             ConnectorCommandRunner
	CodexExecutable    string
	ClaudeExecutable   string
	GrokExecutable     string
	QwenExecutable     string
	QwenHelper         string
	GrokUserPluginRoot string
	ClaudeScope        string
}

// NewNativeConnectorDrivers creates the closed four-product driver inventory.
func NewNativeConnectorDrivers(options NativeConnectorOptions) (map[string]ConnectorDriver, error) {
	runner := options.Runner
	if runner == nil {
		runner = osConnectorCommandRunner{}
	}
	options.Runner = runner
	if options.CodexExecutable == "" {
		options.CodexExecutable = "codex"
	}
	if options.ClaudeExecutable == "" {
		options.ClaudeExecutable = "claude"
	}
	if options.GrokExecutable == "" {
		options.GrokExecutable = "grok"
	}
	if options.QwenExecutable == "" {
		options.QwenExecutable = "qwen"
	}
	if options.ClaudeScope == "" {
		options.ClaudeScope = "user"
	}
	if options.ClaudeScope != "user" {
		return nil, fmt.Errorf("unsupported Claude connector scope %q", options.ClaudeScope)
	}
	if options.GrokUserPluginRoot == "" {
		return nil, errors.New("grok user plugin root is required")
	}
	if !filepath.IsAbs(options.GrokUserPluginRoot) || filepath.Clean(options.GrokUserPluginRoot) != options.GrokUserPluginRoot ||
		filepath.Base(options.GrokUserPluginRoot) != "agent-sessions" {
		return nil, errors.New("grok user plugin root must be a clean absolute .../agent-sessions path")
	}
	return map[string]ConnectorDriver{
		"codex":  newCodexConnectorDriver(options),
		"claude": newClaudeConnectorDriver(options),
		"grok":   newGrokConnectorDriver(options),
		"qwen":   newQwenConnectorDriver(options),
	}, nil
}

type osConnectorCommandRunner struct{}

// LookPath implements ConnectorCommandRunner.
func (osConnectorCommandRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Output implements ConnectorCommandRunner.
func (osConnectorCommandRunner) Output(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...) //nolint:gosec // Executable is resolved from the operator-selected native product boundary; argv is structured.
	var stdout bytes.Buffer
	command.Stdout = &limitedConnectorWriter{writer: &stdout, remaining: 2 * 1024 * 1024}
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, connectorCommandError(executable, args, err)
	}
	return stdout.Bytes(), nil
}

// Run implements ConnectorCommandRunner.
func (osConnectorCommandRunner) Run(ctx context.Context, executable string, args ...string) error {
	command := exec.CommandContext(ctx, executable, args...) //nolint:gosec // Executable is resolved from the operator-selected native product boundary; argv is structured.
	command.Stdin, command.Stdout, command.Stderr = nil, io.Discard, io.Discard
	if err := command.Run(); err != nil {
		return connectorCommandError(executable, args, err)
	}
	return nil
}

type limitedConnectorWriter struct {
	writer    io.Writer
	remaining int
}

func (writer *limitedConnectorWriter) Write(body []byte) (int, error) {
	if len(body) > writer.remaining {
		return 0, errors.New("connector inventory exceeds 2 MiB")
	}
	written, err := writer.writer.Write(body)
	writer.remaining -= written
	return written, err
}

func connectorCommandError(executable string, args []string, cause error) error {
	operation := filepath.Base(executable)
	if len(args) > 0 {
		operation += " " + strings.Join(args[:min(len(args), 2)], " ")
	}
	return fmt.Errorf("native connector command %s failed: %w", operation, cause)
}

type connectorStep struct {
	apply func(context.Context) error
	undo  func(context.Context) error
}

type nativeConnectorMutation struct {
	steps      []connectorStep
	applied    int
	rolledBack bool
}

// Commit implements ConnectorMutation.
func (mutation *nativeConnectorMutation) Commit(ctx context.Context) error {
	if mutation.rolledBack {
		return errors.New("connector mutation was already rolled back")
	}
	for mutation.applied < len(mutation.steps) {
		step := mutation.steps[mutation.applied]
		if err := step.apply(ctx); err != nil {
			return err
		}
		mutation.applied++
	}
	return nil
}

// Rollback implements ConnectorMutation.
func (mutation *nativeConnectorMutation) Rollback(ctx context.Context) error {
	if mutation.rolledBack {
		return nil
	}
	var result error
	for mutation.applied > 0 {
		mutation.applied--
		if undo := mutation.steps[mutation.applied].undo; undo != nil {
			result = errors.Join(result, undo(ctx))
		}
	}
	mutation.rolledBack = true
	return result
}

type noConnectorMutation struct{}

// Commit implements ConnectorMutation.
func (noConnectorMutation) Commit(context.Context) error { return nil }

// Rollback implements ConnectorMutation.
func (noConnectorMutation) Rollback(context.Context) error { return nil }

func resolveOptionalConnector(runner ConnectorCommandRunner, executable string) (string, bool, error) {
	resolved, err := runner.LookPath(executable)
	if err == nil {
		return resolved, true, nil
	}
	// exec.LookPath reports a bare missing command as exec.ErrNotFound, but an
	// explicitly selected missing absolute path as an *exec.Error wrapping
	// os.ErrNotExist. Both describe an unavailable optional product.
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("resolve native executable %q: %w", executable, err)
}

func validateConnectorRequest(request ConnectorRequest, product, payloadRoot string) error {
	want, known := productcatalog.ProductByID(product)
	if !known || request.Product != product || request.Descriptor != want.Connector {
		return errors.New("connector request does not match the authoritative product descriptor")
	}
	if !filepath.IsAbs(request.SourceRoot) || filepath.Clean(request.SourceRoot) != request.SourceRoot ||
		!pathWithin(payloadRoot, request.SourceRoot) {
		return errors.New("connector source is not a clean absolute release payload")
	}
	canonicalSource, err := canonicalConnectorSource(request.SourceRoot, payloadRoot)
	if err != nil {
		return err
	}
	paths := []string{filepath.Join(request.SourceRoot, request.Descriptor.ManifestPath)}
	if strings.Contains(request.Descriptor.EntryPoint, string(filepath.Separator)) {
		paths = append(paths, filepath.Join(request.SourceRoot, request.Descriptor.EntryPoint))
	}
	for _, path := range paths {
		info, pathErr := filepath.EvalSymlinks(path)
		if pathErr != nil || !pathWithin(info, canonicalSource) {
			return fmt.Errorf("connector payload path is missing or escapes the release: %s", path)
		}
	}
	return nil
}

func canonicalConnectorSource(sourceRoot, payloadRoot string) (string, error) {
	canonicalSource, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil || !filepath.IsAbs(canonicalSource) || filepath.Clean(canonicalSource) != canonicalSource {
		return "", errors.New("connector source release payload cannot be canonicalized")
	}
	canonicalPayloadRoot, err := filepath.EvalSymlinks(payloadRoot)
	if err != nil || !pathWithin(canonicalPayloadRoot, canonicalSource) {
		return "", errors.New("connector payload root is missing or escapes the release")
	}
	return canonicalSource, nil
}

func decodeConnectorInventory(body []byte) (any, error) {
	var result any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode native connector inventory: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("native connector inventory contains trailing JSON value")
	}
	return result, nil
}

func connectorObjects(value any) []map[string]any {
	var objects []map[string]any
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			objects = append(objects, typed)
			for _, child := range typed {
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return objects
}

func connectorString(object map[string]any, fields ...string) string {
	for _, field := range fields {
		if value, ok := object[field].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func findConnectorObject(value any, field, wanted string) (map[string]any, bool) {
	for _, object := range connectorObjects(value) {
		if connectorString(object, field) == wanted {
			return object, true
		}
	}
	return nil, false
}

// RunConnectorLifecycleCLI is the transitional explicit product installer
// entry used by source/release Make targets while T031 moves aggregate
// installation onto the canonical host release transaction. It is strict for
// an explicitly selected product and never starts the daemon.
//
//nolint:gocyclo // The strict transitional parser binds four product selectors without a second parser inventory.
func RunConnectorLifecycleCLI(ctx context.Context, operation string, args []string) error {
	values := map[string]string{}
	for len(args) != 0 {
		if len(args) < 2 || !strings.HasPrefix(args[0], "--") || args[1] == "" {
			return errors.New("usage: connector install|remove --product PRODUCT [--source-root ROOT] [--native PATH] [--grok-user-root ROOT]")
		}
		if _, exists := values[args[0]]; exists {
			return fmt.Errorf("duplicate connector option %s", args[0])
		}
		values[args[0]] = args[1]
		args = args[2:]
	}
	product := values["--product"]
	descriptor, known := productcatalog.ProductByID(product)
	if !known || (operation != "install" && operation != "remove") {
		return errors.New("connector lifecycle requires install or remove and one supported product")
	}
	if operation == "install" && (!filepath.IsAbs(values["--source-root"]) || filepath.Clean(values["--source-root"]) != values["--source-root"]) {
		return errors.New("connector install requires a clean absolute --source-root")
	}
	for option := range values {
		switch option {
		case "--product", "--source-root", "--native", "--grok-user-root":
		default:
			return fmt.Errorf("unknown connector option %s", option)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home for connector lifecycle: %w", err)
	}
	options := NativeConnectorOptions{Runner: osConnectorCommandRunner{}, GrokUserPluginRoot: filepath.Join(home, ".grok", "plugins", "agent-sessions")}
	if root := values["--grok-user-root"]; root != "" {
		options.GrokUserPluginRoot = root
	}
	native := values["--native"]
	if native == "" {
		native = product
		if product == "claude" {
			native = "claude"
		}
	}
	switch product {
	case "codex":
		options.CodexExecutable = native
	case "claude":
		options.ClaudeExecutable = native
	case "grok":
		options.GrokExecutable = native
	case "qwen":
		options.QwenExecutable = native
	}
	drivers, err := NewNativeConnectorDrivers(options)
	if err != nil {
		return err
	}
	driver := drivers[product]
	selectedExecutable := map[string]string{
		"codex": options.CodexExecutable, "claude": options.ClaudeExecutable,
		"grok": options.GrokExecutable, "qwen": options.QwenExecutable,
	}[product]
	if _, available, resolveErr := resolveOptionalConnector(options.Runner, selectedExecutable); resolveErr != nil {
		return resolveErr
	} else if !available {
		return fmt.Errorf("selected %s native product is unavailable", descriptor.Label)
	}
	if operation == "remove" {
		return driver.Remove(ctx)
	}
	mutation, err := driver.Prepare(ctx, ConnectorRequest{
		Product: product, SourceRoot: values["--source-root"], Descriptor: descriptor.Connector,
	})
	if err != nil {
		return err
	}
	if err := mutation.Commit(ctx); err != nil {
		return errors.Join(err, mutation.Rollback(ctx))
	}
	return nil
}

type preparedConnector struct {
	product  string
	mutation ConnectorMutation
}

// HostInstallHooks composes the four optional product connector transactions
// around one shared host release transaction.
type HostInstallHooks struct {
	drivers  map[string]ConnectorDriver
	prepared []preparedConnector
}

// NewHostInstallHooks validates one driver per authoritative product.
func NewHostInstallHooks(drivers map[string]ConnectorDriver) (*HostInstallHooks, error) {
	copyDrivers := make(map[string]ConnectorDriver, len(drivers))
	for _, product := range productcatalog.Catalog().Products {
		driver := drivers[product.ID]
		if driver == nil {
			return nil, fmt.Errorf("missing %s connector driver", product.ID)
		}
		copyDrivers[product.ID] = driver
	}
	if len(copyDrivers) != len(drivers) {
		return nil, errors.New("connector driver inventory contains an unknown product")
	}
	return &HostInstallHooks{drivers: copyDrivers}, nil
}

// Prepare stages every installed product connector in authoritative order.
// Individual drivers may implement an explicit no-op mutation when their
// native product is absent.
func (hooks *HostInstallHooks) Prepare(ctx context.Context, request releaseinstall.InstallRequest) error {
	hooks.prepared = nil
	for _, product := range productcatalog.Catalog().Products {
		mutation, err := hooks.drivers[product.ID].Prepare(ctx, ConnectorRequest{
			Product: product.ID, SourceRoot: request.SourceRoot, Descriptor: product.Connector,
		})
		if err != nil {
			rollbackErr := hooks.rollbackPrepared(ctx)
			return errors.Join(fmt.Errorf("prepare %s connector: %w", product.ID, err), rollbackErr)
		}
		if mutation == nil {
			rollbackErr := hooks.rollbackPrepared(ctx)
			return errors.Join(fmt.Errorf("prepare %s connector returned no mutation", product.ID), rollbackErr)
		}
		hooks.prepared = append(hooks.prepared, preparedConnector{product: product.ID, mutation: mutation})
	}
	return nil
}

// Ready is the role hook boundary for exact daemon readiness. Adapter
// readiness is checked by the installed runtime rather than connector drivers.
func (hooks *HostInstallHooks) Ready(context.Context, releaseinstall.InstalledRelease) error {
	return nil
}

// Commit commits prepared connector mutations in authoritative order. Any
// failure restores every exact prior connector state in reverse order.
func (hooks *HostInstallHooks) Commit(ctx context.Context) error {
	for _, prepared := range hooks.prepared {
		if err := prepared.mutation.Commit(ctx); err != nil {
			rollbackErr := hooks.rollbackPrepared(ctx)
			return errors.Join(fmt.Errorf("commit %s connector: %w", prepared.product, err), rollbackErr)
		}
	}
	hooks.prepared = nil
	return nil
}

// Rollback restores every prepared connector in reverse order.
func (hooks *HostInstallHooks) Rollback(ctx context.Context) error {
	return hooks.rollbackPrepared(ctx)
}

// Remove invokes every product's supported native connector removal in the
// same authoritative order and does not delete vendor profiles or state.
func (hooks *HostInstallHooks) Remove(ctx context.Context) error {
	var result error
	for _, product := range productcatalog.Catalog().Products {
		if err := hooks.drivers[product.ID].Remove(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("remove %s connector: %w", product.ID, err))
		}
	}
	return result
}

func (hooks *HostInstallHooks) rollbackPrepared(ctx context.Context) error {
	var result error
	for index := len(hooks.prepared) - 1; index >= 0; index-- {
		prepared := hooks.prepared[index]
		if err := prepared.mutation.Rollback(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("rollback %s connector: %w", prepared.product, err))
		}
	}
	hooks.prepared = nil
	return result
}

// HubInstallHooks returns nil because the central hub owns no native product
// connector lifecycle. Hub orchestration supplies its own role hooks later.
func HubInstallHooks() releaseinstall.RoleHooks { return nil }
