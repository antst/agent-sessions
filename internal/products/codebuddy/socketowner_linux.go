//go:build linux

package codebuddy

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type linuxSocketOwnerVerifier struct{}

func NewPlatformSocketOwnerVerifier() SocketOwnerVerifier { return linuxSocketOwnerVerifier{} }

func (linuxSocketOwnerVerifier) VerifyOwner(ctx context.Context, endpoint string, pid int) (SocketObservation, error) {
	canonical, err := canonicalLoopbackEndpoint(endpoint)
	if err != nil || pid <= 1 {
		return SocketObservation{}, ErrSocketOwner
	}
	parsed, _ := url.Parse(canonical)
	inode, err := linuxListeningInode(ctx, net.ParseIP(parsed.Hostname()), parsed.Port())
	if err != nil {
		return SocketObservation{}, errors.Join(ErrSocketOwner, err)
	}
	owned, err := pidOwnsSocketInode(ctx, pid, inode)
	if err != nil || !owned {
		return SocketObservation{}, errors.Join(ErrSocketOwner, err)
	}
	// Re-read the table after the fd proof so a close/rebind race cannot turn
	// the registry claim into authority for a recycled port.
	current, err := linuxListeningInode(ctx, net.ParseIP(parsed.Hostname()), parsed.Port())
	if err != nil || current != inode {
		return SocketObservation{}, errors.Join(ErrSocketOwner, err)
	}
	owned, err = pidOwnsSocketInode(ctx, pid, current)
	if err != nil || !owned {
		return SocketObservation{}, errors.Join(ErrSocketOwner, err)
	}
	return SocketObservation{PID: pid, Endpoint: canonical, Identity: inode}, nil
}

func linuxListeningInode(ctx context.Context, expectedIP net.IP, expectedPort string) (string, error) {
	wantedPort, err := strconv.Atoi(expectedPort)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(table)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		scanner := bufio.NewScanner(file)
		if scanner.Scan() { // header
		}
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				file.Close()
				return "", ctx.Err()
			default:
			}
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			address := strings.Split(fields[1], ":")
			if len(address) != 2 {
				continue
			}
			port64, portErr := strconv.ParseUint(address[1], 16, 16)
			ip, ipErr := decodeProcNetIP(address[0])
			if portErr == nil && ipErr == nil && int(port64) == wantedPort && ip.Equal(expectedIP) {
				matches = append(matches, fields[9])
			}
		}
		scanErr := scanner.Err()
		file.Close()
		if scanErr != nil {
			return "", scanErr
		}
	}
	if len(matches) != 1 || matches[0] == "" || matches[0] == "0" {
		return "", ErrSocketOwner
	}
	return matches[0], nil
}

func decodeProcNetIP(encoded string) (net.IP, error) {
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != net.IPv4len && len(raw) != net.IPv6len {
		return nil, ErrSocketOwner
	}
	if len(raw) == net.IPv4len {
		for left, right := 0, len(raw)-1; left < right; left, right = left+1, right-1 {
			raw[left], raw[right] = raw[right], raw[left]
		}
	} else {
		for offset := 0; offset < len(raw); offset += 4 {
			raw[offset], raw[offset+3] = raw[offset+3], raw[offset]
			raw[offset+1], raw[offset+2] = raw[offset+2], raw[offset+1]
		}
	}
	return net.IP(raw), nil
}

func pidOwnsSocketInode(ctx context.Context, pid int, inode string) (bool, error) {
	directory := "/proc/" + strconv.Itoa(pid) + "/fd"
	handle, err := os.Open(directory)
	if err != nil {
		return false, err
	}
	defer handle.Close()
	const maximumFDs = 1 << 16
	entries, err := handle.Readdirnames(maximumFDs + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if len(entries) > maximumFDs {
		return false, ErrSocketOwner
	}
	wanted := "socket:[" + inode + "]"
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		target, err := os.Readlink(filepath.Join(directory, entry))
		if err == nil && target == wanted {
			return true, nil
		}
	}
	return false, nil
}
