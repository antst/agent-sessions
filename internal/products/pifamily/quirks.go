// Package pifamily contains only the native mechanics that Pi and Oh My Pi
// actually share. Product policy and composition remain in the pi and omp
// packages.
package pifamily

import (
	"errors"
	"fmt"
	"strings"
)

const (
	PiProductID  = "pi"
	OMPProductID = "omp"

	PiTestedVersion  = "0.84.4"
	OMPTestedVersion = "18.0.11"

	IntegrationVersion = "1"
	MaxRPCFrameBytes   = 1 << 20
	MaxPromptBytes     = 1 << 20
)

// ReadyStrategy records a real protocol difference. Pi 0.84.4 has no ready
// frame, so readiness is established by a correlated get_state response. OMP
// 18.0.11 emits a version-advertising ready frame before accepting commands.
type ReadyStrategy string

const (
	ReadyByStateProbe ReadyStrategy = "state-probe"
	ReadyByEvent      ReadyStrategy = "ready-event"
)

// ArgStyle records the supported spelling of an option for a pinned product.
type ArgStyle string

const (
	ArgSeparate ArgStyle = "separate"
	ArgEquals   ArgStyle = "equals"
)

// Quirks is the explicit, closed Pi-family difference table. It intentionally
// does not provide a permissive default for an unknown product.
type Quirks struct {
	ProductID          string
	Executable         string
	Runtime            string
	TestedVersion      string
	AgentDirectory     string
	ExtensionArgStyle  ArgStyle
	ModeArgStyle       ArgStyle
	ReadyStrategy      ReadyStrategy
	TerminalEvent      string
	NativeSteerFraming bool
	NativeSessionEnv   bool
	ResumeFlag         string
	DefaultPolicy      string
	BypassPolicy       string
}

var quirkTable = map[string]Quirks{
	PiProductID: {
		ProductID: PiProductID, Executable: "pi", Runtime: "node", TestedVersion: PiTestedVersion,
		AgentDirectory: ".pi/agent", ExtensionArgStyle: ArgSeparate, ModeArgStyle: ArgSeparate,
		ReadyStrategy: ReadyByStateProbe, TerminalEvent: "agent_settled", NativeSteerFraming: false,
		NativeSessionEnv: true, ResumeFlag: "--session", DefaultPolicy: "restricted-tools",
		BypassPolicy: "full-tools",
	},
	OMPProductID: {
		ProductID: OMPProductID, Executable: "omp", Runtime: "bun", TestedVersion: OMPTestedVersion,
		AgentDirectory: ".omp/agent", ExtensionArgStyle: ArgEquals, ModeArgStyle: ArgEquals,
		ReadyStrategy: ReadyByEvent, TerminalEvent: "agent_end", NativeSteerFraming: true,
		NativeSessionEnv: false, ResumeFlag: "--session", DefaultPolicy: "unsupported-rpc-approval",
		BypassPolicy: "yolo",
	},
}

// QuirksFor returns a copy of the closed quirk-table row.
func QuirksFor(productID string) (Quirks, error) {
	quirks, ok := quirkTable[productID]
	if !ok {
		return Quirks{}, fmt.Errorf("%w: unknown Pi-family product %q", ErrUnknownProduct, productID)
	}
	return quirks, nil
}

// Validate rejects partially constructed rows so tests and composition cannot
// accidentally weaken one product by manufacturing an incomplete table row.
func (q Quirks) Validate() error {
	want, ok := quirkTable[q.ProductID]
	if !ok {
		return fmt.Errorf("%w: unknown Pi-family product %q", ErrUnknownProduct, q.ProductID)
	}
	if q != want {
		return errors.New("Pi-family quirk row does not exactly match the supported table")
	}
	return nil
}

func option(style ArgStyle, name, value string) []string {
	if style == ArgEquals {
		return []string{name + "=" + value}
	}
	return []string{name, value}
}

func (q Quirks) modeArguments() []string {
	return option(q.ModeArgStyle, "--mode", "rpc")
}

func (q Quirks) extensionArguments(path string) []string {
	return option(q.ExtensionArgStyle, "--extension", path)
}

func (q Quirks) resumeArguments(nativeSessionID string) []string {
	return []string{q.ResumeFlag, nativeSessionID}
}

func reservedArgument(argument string) bool {
	name := argument
	if index := strings.IndexByte(name, '='); index >= 0 {
		name = name[:index]
	}
	switch name {
	case "--mode", "--extension", "-e", "--session", "-r", "--resume", "--session-id",
		"--session-dir", "--name", "-n", "--tools", "--exclude-tools", "--no-tools",
		"--approval-mode", "--auto-approve", "--yolo":
		return true
	default:
		return argument == "--"
	}
}

var ErrUnknownProduct = errors.New("unknown Pi-family product")
