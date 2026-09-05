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
	"time"
)

const stderrLimit = 64 << 10

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, value...)
	if len(b.data) > stderrLimit {
		b.data = append([]byte(nil), b.data[len(b.data)-stderrLimit:]...)
	}
	b.mu.Unlock()
	return len(value), nil
}

func (b *tailBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}

type Process struct {
	cmd  *exec.Cmd
	done chan struct{}
	tail *tailBuffer
	mu   sync.Mutex
	exit int
	stop sync.Once
}

func Start(path string, environment []string) (*Process, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	command := exec.Command(path)
	command.Env, command.Stderr = environment, write
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, err
	}
	_ = write.Close()
	p := &Process{cmd: command, done: make(chan struct{}), tail: &tailBuffer{}, exit: -1}
	go func() { _, _ = io.Copy(p.tail, read); _ = read.Close() }()
	go func() {
		_ = command.Wait()
		p.mu.Lock()
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
	exit := p.exit
	p.mu.Unlock()
	return splitLines(p.tail.bytes()), exit
}

func (p *Process) signal(signal syscall.Signal) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = p.cmd.Process.Signal(signal)
	}
}

// Stop signals the owned group, reaps the direct child, then kills any
// descendants that survived it. It runs once for each process.
func (p *Process) Stop(grace time.Duration) {
	if p == nil {
		return
	}
	p.stop.Do(func() {
		if grace > 0 {
			p.signal(syscall.SIGTERM)
			timer := time.NewTimer(grace)
			select {
			case <-p.done:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				p.signal(syscall.SIGKILL)
				<-p.done
			}
		} else {
			p.signal(syscall.SIGKILL)
			<-p.done
		}
		p.signal(syscall.SIGKILL)
	})
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
