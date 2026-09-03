// Command releasetool provides repository-only release packaging and evidence operations.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/antst/agent-sessions/internal/releaseevidence"
	"github.com/antst/agent-sessions/internal/releasepkg"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "releasetool: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("requires package or evidence")
	}
	switch args[0] {
	case "package":
		if len(args) != 4 {
			return errors.New("package requires STAGE_ROOT PACKAGE_NAME ARCHIVE")
		}
		return releasepkg.Create(args[1], args[2], args[3])
	case "evidence":
		return runEvidence(args[1:])
	default:
		return fmt.Errorf("unknown operation %q", args[0])
	}
}

func runEvidence(args []string) error {
	if len(args) == 0 {
		return errors.New("evidence requires generate, canonicalize, or validate")
	}
	switch args[0] {
	case "generate":
		values, err := exactNamedArguments(args[1:], "schema", "inventory", "platforms", "archive-dir", "package-dir", "gate-dir", "linux-gate", "macos-gate", "output", "version", "commit", "tree", "run-id", "run-attempt", "run-url")
		if err != nil {
			return err
		}
		runID, err := positiveInt64(values["run-id"])
		if err != nil {
			return fmt.Errorf("run-id: %w", err)
		}
		runAttempt, err := positiveInt64(values["run-attempt"])
		if err != nil {
			return fmt.Errorf("run-attempt: %w", err)
		}
		return releaseevidence.Generate(releaseevidence.GenerateOptions{
			SchemaPath: values["schema"], InventoryPath: values["inventory"],
			PlatformsPath: values["platforms"], ArchiveDir: values["archive-dir"],
			PackageDir: values["package-dir"],
			GateDir:    values["gate-dir"], LinuxGatePath: values["linux-gate"],
			MacOSGatePath: values["macos-gate"], OutputPath: values["output"],
			Version: values["version"], Commit: values["commit"], Tree: values["tree"],
			RunID: runID, RunAttempt: runAttempt, RunURL: values["run-url"],
		})
	case "canonicalize":
		values, err := exactNamedArguments(args[1:], "schema", "input", "output")
		if err != nil {
			return err
		}
		return releaseevidence.Canonicalize(values["schema"], values["input"], values["output"])
	case "validate":
		values, err := exactNamedArguments(args[1:], "schema", "document", "archive-dir", "package-dir", "gate-dir", "commit", "tree", "run-id")
		if err != nil {
			return err
		}
		runID, err := positiveInt64(values["run-id"])
		if err != nil {
			return fmt.Errorf("run-id: %w", err)
		}
		return releaseevidence.CrossCheck(values["schema"], values["document"], values["archive-dir"], values["package-dir"], values["gate-dir"], values["commit"], values["tree"], runID)
	default:
		return fmt.Errorf("unknown evidence operation %q", args[0])
	}
}

func positiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("must be a positive integer")
	}
	return parsed, nil
}

func exactNamedArguments(args []string, names ...string) (map[string]string, error) {
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	result := make(map[string]string, len(names))
	remaining := args
	for len(remaining) > 0 {
		if len(remaining) < 2 {
			return nil, errors.New("every option requires one value")
		}
		option, value := remaining[0], remaining[1]
		remaining = remaining[2:]
		if len(option) < 3 || option[:2] != "--" {
			return nil, errors.New("every option requires one value")
		}
		name := option[2:]
		if !allowed[name] || result[name] != "" || value == "" {
			return nil, fmt.Errorf("unknown, duplicate, or empty option --%s", name)
		}
		result[name] = value
	}
	for _, name := range names {
		if result[name] == "" {
			return nil, fmt.Errorf("missing --%s", name)
		}
	}
	return result, nil
}
