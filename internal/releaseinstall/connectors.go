package releaseinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Product names one supported vendor-native connector installer.
type Product string

const (
	// CodexProduct selects the Codex native connector.
	CodexProduct Product = "codex"
	// ClaudeProduct selects the Claude native connector.
	ClaudeProduct Product = "claude"
	// GrokProduct selects the Grok native connector.
	GrokProduct Product = "grok"
	// QwenProduct selects the Qwen native connector.
	QwenProduct Product = "qwen"
)

var connectorProducts = []Product{CodexProduct, ClaudeProduct, GrokProduct, QwenProduct}

// ConnectorRunner exposes only native product availability and command
// execution. Credential, history, transcript, and settings APIs are absent by
// construction.
type ConnectorRunner interface {
	// Available reports whether the selected native product can be managed.
	Available(Product) bool
	// Run executes fixed native connector argv for the selected product.
	Run(context.Context, Product, ...string) error
}

// ConnectorTransaction installs or removes the four optional native
// integrations. Stat is injectable for access-boundary verification.
type ConnectorTransaction struct {
	SourceRoot string
	Runner     ConnectorRunner
	Stat       func(string) (os.FileInfo, error)
}

// Install validates every available product source before the first native
// mutation and compensates in reverse order on failure.
func (transaction ConnectorTransaction) Install(ctx context.Context) error {
	if err := transaction.validate(); err != nil {
		return err
	}
	available := make([]Product, 0, len(connectorProducts))
	for _, product := range connectorProducts {
		if transaction.Runner.Available(product) {
			available = append(available, product)
		}
	}
	for _, product := range available {
		if err := transaction.validateSource(product); err != nil {
			return err
		}
	}
	installed := make([]Product, 0, len(available))
	for _, product := range available {
		if err := transaction.installProduct(ctx, product); err != nil {
			rollbackErr := transaction.removeProduct(ctx, product)
			for index := len(installed) - 1; index >= 0; index-- {
				rollbackErr = errors.Join(rollbackErr, transaction.removeProduct(ctx, installed[index]))
			}
			return errors.Join(fmt.Errorf("install %s connector: %w", product, err), rollbackErr)
		}
		installed = append(installed, product)
	}
	return nil
}

// Remove invokes exact native removal in reverse installation order. Missing
// products are optional, and all present products are attempted.
func (transaction ConnectorTransaction) Remove(ctx context.Context) error {
	if err := transaction.validate(); err != nil {
		return err
	}
	var result error
	for index := len(connectorProducts) - 1; index >= 0; index-- {
		product := connectorProducts[index]
		if transaction.Runner.Available(product) {
			result = errors.Join(result, transaction.removeProduct(ctx, product))
		}
	}
	return result
}

func (transaction ConnectorTransaction) validate() error {
	if transaction.Runner == nil {
		return errors.New("connector runner is required")
	}
	if !filepath.IsAbs(transaction.SourceRoot) {
		return errors.New("connector source root must be absolute")
	}
	return nil
}

func (transaction ConnectorTransaction) validateSource(product Product) error {
	path := transaction.validationPath(product)
	stat := transaction.Stat
	if stat == nil {
		stat = os.Lstat
	}
	info, err := stat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s connector source is not a real directory: %s", product, path)
	}
	return nil
}

func (transaction ConnectorTransaction) validationPath(product Product) string {
	switch product {
	case CodexProduct:
		return transaction.SourceRoot
	case ClaudeProduct:
		return filepath.Join(transaction.SourceRoot, "claude")
	case GrokProduct:
		return filepath.Join(transaction.SourceRoot, "grok")
	case QwenProduct:
		return filepath.Join(transaction.SourceRoot, "qwen")
	default:
		return ""
	}
}

func (transaction ConnectorTransaction) installProduct(ctx context.Context, product Product) error {
	run := func(args ...string) error { return transaction.Runner.Run(ctx, product, args...) }
	switch product {
	case CodexProduct:
		if err := run("plugin", "marketplace", "add", transaction.SourceRoot); err != nil {
			return err
		}
		return run("plugin", "add", "agent-sessions@agent-sessions")
	case ClaudeProduct:
		if err := run("plugin", "marketplace", "add", "--scope", "user", transaction.SourceRoot); err != nil {
			return err
		}
		return run("plugin", "install", "--scope", "user", "--yes", "agent-sessions@agent-sessions")
	case GrokProduct:
		return run("plugin", "install", filepath.Join(transaction.SourceRoot, "grok"), "--trust")
	case QwenProduct:
		return run("extensions", "install", filepath.Join(transaction.SourceRoot, "qwen"), "--consent", "--scope", "user")
	default:
		return fmt.Errorf("unsupported connector product %q", product)
	}
}

func (transaction ConnectorTransaction) removeProduct(ctx context.Context, product Product) error {
	run := func(args ...string) error { return transaction.Runner.Run(ctx, product, args...) }
	switch product {
	case CodexProduct:
		return errors.Join(
			run("plugin", "remove", "agent-sessions@agent-sessions"),
			run("plugin", "marketplace", "remove", "agent-sessions"),
		)
	case ClaudeProduct:
		return errors.Join(
			run("plugin", "uninstall", "--scope", "user", "--keep-data", "agent-sessions@agent-sessions"),
			run("plugin", "marketplace", "remove", "--scope", "user", "agent-sessions"),
		)
	case GrokProduct:
		return run("plugin", "uninstall", "agent-sessions", "--keep-data")
	case QwenProduct:
		return run("extensions", "uninstall", "agent-sessions")
	default:
		return fmt.Errorf("unsupported connector product %q", product)
	}
}
