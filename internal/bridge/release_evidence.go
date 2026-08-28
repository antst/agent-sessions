package bridge

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/antst/agent-sessions/internal/releaseevidence"
)

// RunReleaseEvidence executes the repository-internal release evidence helper
// from the canonical host image. It is intentionally absent from public help.
//
//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func RunReleaseEvidence(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: release-evidence generate|canonicalize|validate ...")
		return 2
	}
	switch args[0] {
	case "generate":
		values, err := exactNamedArguments(args[1:], "schema", "inventory", "platforms", "archive-dir", "gate-dir", "linux-gate", "macos-gate", "output", "version", "commit", "tree", "run-id", "run-attempt", "run-url")
		var runID, runAttempt int64
		if err == nil {
			runID, err = strconv.ParseInt(values["run-id"], 10, 64)
		}
		if err == nil {
			runAttempt, err = strconv.ParseInt(values["run-attempt"], 10, 64)
		}
		if err == nil {
			err = releaseevidence.Generate(releaseevidence.GenerateOptions{
				SchemaPath: values["schema"], InventoryPath: values["inventory"],
				PlatformsPath: values["platforms"], ArchiveDir: values["archive-dir"],
				GateDir: values["gate-dir"], LinuxGatePath: values["linux-gate"],
				MacOSGatePath: values["macos-gate"], OutputPath: values["output"],
				Version: values["version"], Commit: values["commit"], Tree: values["tree"],
				RunID: runID, RunAttempt: runAttempt, RunURL: values["run-url"],
			})
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "release-evidence generate: %v\n", err)
			return 1
		}
		return 0
	case "canonicalize":
		values, err := exactNamedArguments(args[1:], "schema", "input", "output")
		if err == nil {
			err = releaseevidence.Canonicalize(values["schema"], values["input"], values["output"])
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "release-evidence canonicalize: %v\n", err)
			return 1
		}
		return 0
	case "validate":
		values, err := exactNamedArguments(args[1:], "schema", "document", "archive-dir", "gate-dir", "commit", "tree", "run-id")
		var runID int64
		if err == nil {
			runID, err = strconv.ParseInt(values["run-id"], 10, 64)
			if err == nil && runID <= 0 {
				err = errors.New("run-id must be positive")
			}
		}
		if err == nil {
			err = releaseevidence.CrossCheck(values["schema"], values["document"], values["archive-dir"], values["gate-dir"], values["commit"], values["tree"], runID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "release-evidence validate: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "release-evidence: unknown command %q\n", args[0])
		return 2
	}
}

func exactNamedArguments(args []string, names ...string) (map[string]string, error) {
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	result := make(map[string]string, len(names))
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) || len(args[index]) < 3 || args[index][:2] != "--" {
			return nil, errors.New("every release-evidence option requires one value")
		}
		name := args[index][2:]
		if !allowed[name] || result[name] != "" || args[index+1] == "" {
			return nil, fmt.Errorf("unknown, duplicate, or empty option --%s", name)
		}
		result[name] = args[index+1]
		index++
	}
	for _, name := range names {
		if result[name] == "" {
			return nil, fmt.Errorf("missing --%s", name)
		}
	}
	return result, nil
}
