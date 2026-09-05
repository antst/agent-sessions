package daemon

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

const stderrLimit = 64 << 10

type process struct {
	cmd    *exec.Cmd
	done   chan struct{}
	stderr *tailBuffer
	exit   int
	err    error
}

type tailBuffer struct {
	mu  sync.Mutex
	raw []byte
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	b.raw = append(b.raw, value...)
	if len(b.raw) > stderrLimit {
		b.raw = append([]byte(nil), b.raw[len(b.raw)-stderrLimit:]...)
	}
	b.mu.Unlock()
	return len(value), nil
}

func (b *tailBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(bytes.ToValidUTF8(b.raw, []byte("?")))), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}
	}
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return append([]string(nil), lines...)
}

func startProcess(path string, environment []string, output io.Writer) (*process, error) {
	command := exec.Command(path)
	command.Env = environment
	command.Stdout = output
	stderr := &tailBuffer{}
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	p := &process{cmd: command, done: make(chan struct{}), stderr: stderr}
	go func() {
		p.err = command.Wait()
		if command.ProcessState != nil {
			p.exit = command.ProcessState.ExitCode()
		}
		close(p.done)
	}()
	return p, nil
}

func (p *process) signal(signal syscall.Signal) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = p.cmd.Process.Signal(signal)
	}
}

func (p *process) killAndWait() {
	if p == nil {
		return
	}
	select {
	case <-p.done:
	default:
		p.signal(syscall.SIGKILL)
		<-p.done
	}
}

func replaceEnvironment(base []string, values map[string]string) []string {
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
