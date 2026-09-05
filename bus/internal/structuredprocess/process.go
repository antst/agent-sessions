package structuredprocess

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
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
	stop    atomic.Bool
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
		_ = readEnd.Close()
		raw := <-tail
		exit := -1
		if command.ProcessState != nil {
			exit = command.ProcessState.ExitCode()
		}
		p.details = processDetails{stderr: splitLines(raw), exit: exit}
		p.signal(syscall.SIGKILL)
		close(p.done)
	}()
	return p, nil
}

func readTail(reader io.Reader, result chan<- []byte) {
	buffer := make([]byte, 4096)
	var tail []byte
	for {
		count, err := reader.Read(buffer)
		if count != 0 {
			tail = append(tail, buffer[:count]...)
			if len(tail) > stderrLimit {
				tail = append([]byte(nil), tail[len(tail)-stderrLimit:]...)
			}
		}
		if err != nil {
			result <- tail
			return
		}
	}
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

func (p *Process) Stop(bound time.Duration) {
	if p == nil {
		return
	}
	if p.stop.CompareAndSwap(false, true) {
		p.signal(syscall.SIGTERM)
		timer := time.NewTimer(bound)
		select {
		case <-p.done:
			timer.Stop()
		case <-timer.C:
			p.signal(syscall.SIGKILL)
			<-p.done
		}
		return
	}
	<-p.done
}

func (p *Process) KillAndWait() { p.Stop(0) }

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
