package structuredprocess

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const stderrLimit = 64 << 10

type processDetails struct {
	stderr []string
	exit   int
}

type Process struct {
	cmd     *exec.Cmd
	done    chan struct{}
	details processDetails
}

func Start(path string, environment []string) (*Process, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	command := exec.Command(path)
	command.Env, command.Stderr = environment, writeEnd
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = command.Start(); err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return nil, err
	}
	_ = writeEnd.Close()
	tail := make(chan []byte, 1)
	go readTail(readEnd, tail)
	p := &Process{cmd: command, done: make(chan struct{})}
	go func() {
		_ = command.Wait()
		_ = readEnd.SetReadDeadline(time.Now())
		raw := <-tail
		exit := -1
		if command.ProcessState != nil {
			exit = command.ProcessState.ExitCode()
		}
		p.details = processDetails{stderr: splitLines(raw), exit: exit}
		close(p.done)
	}()
	return p, nil
}

func readTail(reader *os.File, result chan<- []byte) {
	defer reader.Close()
	buffer := make([]byte, 4096)
	var tail []byte
	for {
		count, err := reader.Read(buffer)
		if count != 0 {
			tail = appendTail(tail, buffer[:count])
		}
		if err != nil {
			tail = drainTail(reader, tail, buffer)
			result <- tail
			return
		}
	}
}

func drainTail(reader *os.File, tail, buffer []byte) []byte {
	raw, err := reader.SyscallConn()
	if err != nil {
		return tail
	}
	_ = reader.SetReadDeadline(time.Time{})
	_ = raw.Read(func(fd uintptr) bool {
		for {
			count, readErr := syscall.Read(int(fd), buffer)
			if count > 0 {
				tail = appendTail(tail, buffer[:count])
			}
			if readErr == syscall.EINTR {
				continue
			}
			if readErr == nil && count > 0 {
				continue
			}
			return true
		}
	})
	return tail
}

func appendTail(tail, part []byte) []byte {
	tail = append(tail, part...)
	if len(tail) > stderrLimit {
		tail = append([]byte(nil), tail[len(tail)-stderrLimit:]...)
	}
	return tail
}

func splitLines(raw []byte) []string {
	lines := strings.Split(strings.TrimSpace(string(bytes.ToValidUTF8(raw, []byte("?")))), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return lines
}

func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) Details() ([]string, int) {
	select {
	case <-p.done:
		return append([]string(nil), p.details.stderr...), p.details.exit
	default:
		return nil, -1
	}
}

func (p *Process) Signal(signal syscall.Signal) { p.signal(signal) }

func (p *Process) signal(signal syscall.Signal) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = p.cmd.Process.Signal(signal)
	}
}

func (p *Process) Stop() {
	if p == nil {
		return
	}
	p.signal(syscall.SIGTERM)
	p.signal(syscall.SIGKILL)
	<-p.done
}

func Environment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, variable := range base {
		name, _, _ := strings.Cut(variable, "=")
		if _, replaced := values[name]; !replaced {
			result = append(result, variable)
		}
	}
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}
