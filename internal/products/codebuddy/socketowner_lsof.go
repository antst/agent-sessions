package codebuddy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const darwinLsofPath = "/usr/sbin/lsof"

// lsofSocketOwnerVerifier is the pure-Go, CGO-independent Darwin release
// verifier. It uses the OS-shipped lsof binary with field output and validates
// the exact PID, listening endpoint, descriptor, device, and node twice.
type lsofSocketOwnerVerifier struct{ runner CommandRunner }

func newLsofSocketOwnerVerifier(runner CommandRunner) SocketOwnerVerifier {
	return &lsofSocketOwnerVerifier{runner: runner}
}

func (verifier *lsofSocketOwnerVerifier) VerifyOwner(ctx context.Context, endpoint string, pid int) (SocketObservation, error) {
	canonical, err := canonicalLoopbackEndpoint(endpoint)
	if err != nil || pid <= 1 || verifier == nil || verifier.runner == nil || ctx == nil {
		return SocketObservation{}, ErrSocketOwner
	}
	parsed, _ := url.Parse(canonical)
	target := net.JoinHostPort(net.ParseIP(parsed.Hostname()).String(), parsed.Port())
	arguments := []string{
		"-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP@" + target, "-sTCP:LISTEN", "-F0pftPDinT",
	}
	observe := func() (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		result, runErr := verifier.runner.Run(ctx, darwinLsofPath, arguments...)
		if runErr != nil || len(result.Stdout) == 0 || len(result.Stdout) >= 64<<10 {
			return "", errors.Join(ErrSocketOwner, runErr)
		}
		return parseLsofSocketIdentity(result.Stdout, pid, target)
	}
	first, err := observe()
	if err != nil {
		return SocketObservation{}, err
	}
	second, err := observe()
	if err != nil || second != first {
		return SocketObservation{}, errors.Join(ErrSocketOwner, err)
	}
	return SocketObservation{PID: pid, Endpoint: canonical, Identity: first}, nil
}

type lsofSocketRecord struct {
	pid      int
	fd       string
	family   string
	protocol string
	device   string
	node     string
	name     string
	state    string
}

func parseLsofSocketIdentity(output string, wantedPID int, target string) (string, error) {
	if len(output) == 0 || len(output) >= 64<<10 || wantedPID <= 1 || target == "" {
		return "", ErrSocketOwner
	}
	fields := strings.FieldsFunc(output, func(r rune) bool { return r == 0 || r == '\n' || r == '\r' })
	currentPID := 0
	processRecords := 0
	fdRecords := 0
	var current *lsofSocketRecord
	var matches []lsofSocketRecord
	flush := func() {
		if current == nil {
			return
		}
		name := strings.TrimSuffix(current.name, " (LISTEN)")
		if current.pid == wantedPID && allDecimal(current.fd) &&
			(current.family == "IPv4" || current.family == "IPv6") && current.protocol == "TCP" &&
			current.device != "" && current.node != "" && name == target && current.state == "ST=LISTEN" {
			matches = append(matches, *current)
		}
		current = nil
	}
	for _, field := range fields {
		if len(field) < 2 {
			continue
		}
		kind, value := field[0], field[1:]
		switch kind {
		case 'p':
			flush()
			currentPID, _ = strconv.Atoi(value)
			processRecords++
		case 'f':
			flush()
			current = &lsofSocketRecord{pid: currentPID, fd: value}
			if currentPID == wantedPID {
				fdRecords++
			}
		case 't':
			if current != nil {
				current.family = value
			}
		case 'P':
			if current != nil {
				current.protocol = value
			}
		case 'D':
			if current != nil {
				current.device = value
			}
		case 'i':
			if current != nil {
				current.node = value
			}
		case 'n':
			if current != nil {
				current.name = value
			}
		case 'T':
			if current != nil && strings.HasPrefix(value, "ST=") {
				current.state = value
			}
		}
	}
	flush()
	if len(matches) != 1 || processRecords != 1 || fdRecords != 1 {
		return "", ErrSocketOwner
	}
	match := matches[0]
	return fmt.Sprintf("lsof:%d:%s:%s:%s:%s", wantedPID, match.fd, match.device, match.node, target), nil
}

func allDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
