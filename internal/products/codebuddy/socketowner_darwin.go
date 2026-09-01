//go:build darwin && cgo

package codebuddy

/*
#cgo LDFLAGS: -lproc
#include <arpa/inet.h>
#include <libproc.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>

static int codebuddy_listening_fd(pid_t pid, int family, const unsigned char *address, int port) {
	int bytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (bytes <= 0 || bytes > (1 << 24)) return -1;
	struct proc_fdinfo *fds = (struct proc_fdinfo *)calloc(1, (size_t)bytes);
	if (fds == NULL) return -1;
	bytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, bytes);
	if (bytes <= 0) { free(fds); return -1; }
	int count = bytes / (int)sizeof(struct proc_fdinfo);
	int match = -1;
	for (int index = 0; index < count; index++) {
		if (fds[index].proc_fdtype != PROX_FDTYPE_SOCKET) continue;
		struct socket_fdinfo info;
		memset(&info, 0, sizeof(info));
		int got = proc_pidfdinfo(pid, fds[index].proc_fd, PROC_PIDFDSOCKETINFO, &info, sizeof(info));
		if (got != sizeof(info) || info.psi.soi_kind != SOCKINFO_TCP) continue;
		struct tcp_sockinfo *tcp = &info.psi.soi_proto.pri_tcp;
		struct in_sockinfo *in = &tcp->tcpsi_ini;
		if (tcp->tcpsi_state != TSI_S_LISTEN || ntohs((uint16_t)in->insi_lport) != port) continue;
		int address_match = 0;
		if (family == AF_INET && (in->insi_vflag & INI_IPV4)) {
			address_match = memcmp(&in->insi_laddr.ina_46.i46a_addr4, address, 4) == 0;
		} else if (family == AF_INET6 && (in->insi_vflag & INI_IPV6)) {
			address_match = memcmp(&in->insi_laddr.ina_6, address, 16) == 0;
		}
		if (!address_match) continue;
		if (match != -1) { free(fds); return -2; }
		match = fds[index].proc_fd;
	}
	free(fds);
	return match;
}
*/
import "C"

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"unsafe"
)

type darwinSocketOwnerVerifier struct{}

func NewPlatformSocketOwnerVerifier() SocketOwnerVerifier { return darwinSocketOwnerVerifier{} }

func (darwinSocketOwnerVerifier) VerifyOwner(ctx context.Context, endpoint string, pid int) (SocketObservation, error) {
	canonical, err := canonicalLoopbackEndpoint(endpoint)
	if err != nil || pid <= 1 {
		return SocketObservation{}, ErrSocketOwner
	}
	select {
	case <-ctx.Done():
		return SocketObservation{}, ctx.Err()
	default:
	}
	parsed, _ := url.Parse(canonical)
	ip := net.ParseIP(parsed.Hostname())
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return SocketObservation{}, ErrSocketOwner
	}
	family := C.int(C.AF_INET6)
	address := []byte(ip.To16())
	if ipv4 := ip.To4(); ipv4 != nil {
		family = C.int(C.AF_INET)
		address = []byte(ipv4)
	}
	fd := int(C.codebuddy_listening_fd(C.pid_t(pid), family, (*C.uchar)(unsafe.Pointer(&address[0])), C.int(port)))
	if fd < 0 {
		return SocketObservation{}, ErrSocketOwner
	}
	// Repeat after the first complete proc snapshot to reject a close/rebind
	// race or fd recycling during attestation.
	select {
	case <-ctx.Done():
		return SocketObservation{}, ctx.Err()
	default:
	}
	second := int(C.codebuddy_listening_fd(C.pid_t(pid), family, (*C.uchar)(unsafe.Pointer(&address[0])), C.int(port)))
	if second != fd {
		return SocketObservation{}, ErrSocketOwner
	}
	return SocketObservation{PID: pid, Endpoint: canonical, Identity: fmt.Sprintf("fd:%d", fd)}, nil
}
