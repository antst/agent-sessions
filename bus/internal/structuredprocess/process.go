package structuredprocess

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

const stderrLimit = 64 << 10

type Process struct {
	cmd    *exec.Cmd
	done   chan struct{}
	mu     sync.Mutex
	stderr []string
	exit   int
}

func Start(path string, environment []string) (*Process, error) {
	stderr, err := os.CreateTemp("", ".agentbus-stderr-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(stderr.Name())
	command := exec.Command(path)
	command.Env, command.Stderr = environment, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = stderr.Close()
		return nil, err
	}
	p := &Process{cmd: command, done: make(chan struct{}), exit: -1}
	go func() {
		_ = command.Wait()
		end, _ := stderr.Seek(0, io.SeekEnd)
		_, _ = stderr.Seek(max(0, end-stderrLimit), io.SeekStart)
		raw, _ := io.ReadAll(stderr)
		_ = stderr.Close()
		p.mu.Lock()
		p.stderr = splitLines(raw)
		if command.ProcessState != nil {
			p.exit = command.ProcessState.ExitCode()
		}
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.stderr...), p.exit
}

func (p *Process) Signal(signal syscall.Signal) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = p.cmd.Process.Signal(signal)
	}
}

func (p *Process) KillAndWait() {
	if p == nil {
		return
	}
	select {
	case <-p.done:
	default:
		p.Signal(syscall.SIGKILL)
		<-p.done
	}
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
