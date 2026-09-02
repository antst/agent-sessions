package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/clihelp"
	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/servicecontrol"
)

type recordingServiceManager struct{ actions []string }

func (manager *recordingServiceManager) Start(context.Context) error {
	manager.actions = append(manager.actions, "start")
	return nil
}

func (manager *recordingServiceManager) Stop(context.Context) error {
	manager.actions = append(manager.actions, "stop")
	return nil
}

func (manager *recordingServiceManager) Restart(context.Context) error {
	manager.actions = append(manager.actions, "restart")
	return nil
}

func TestRunDispatchesEveryCommandAndAliasExactlyOnce(t *testing.T) {
	commands := []string{"daemon", "status", "doctor", "roster", "catalog", "peer", "lane", "hook", "connector"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			calls := map[string]int{}
			var got clihelp.Invocation
			runners := commandRunners{}
			set := func(name string) commandRunner {
				return func(_ context.Context, invocation clihelp.Invocation, _ io.Writer) error {
					calls[name]++
					got = invocation
					return nil
				}
			}
			runners.daemon, runners.status, runners.doctor, runners.roster = set("daemon"), set("status"), set("doctor"), set("roster")
			runners.catalog = set("catalog")
			runners.peer, runners.lane, runners.hook, runners.connector = set("peer"), set("lane"), set("hook"), set("connector")
			args := []string{command, "codex", "--", "native", "bytes"}
			if command == "daemon" || command == "status" || command == "doctor" || command == "roster" {
				args = []string{command, "--state-root", "/state"}
			} else if command == "catalog" {
				args = []string{command, "--json"}
			}
			var output bytes.Buffer
			if err := run(context.Background(), "agent-sessions", args, &output, runners); err != nil {
				t.Fatal(err)
			}
			if calls[command] != 1 || len(calls) != 1 || got.Command != command {
				t.Fatalf("dispatch calls=%v invocation=%+v", calls, got)
			}
		})
	}

	var got clihelp.Invocation
	runners := commandRunners{peer: func(_ context.Context, invocation clihelp.Invocation, _ io.Writer) error {
		got = invocation
		return nil
	}}
	if err := run(context.Background(), "/links/claude-peer", []string{"--resume", "native title", "--", "prompt"}, &bytes.Buffer{}, runners); err != nil {
		t.Fatal(err)
	}
	if got.Command != "peer" || got.Product != "claude" || !reflect.DeepEqual(got.Arguments, []string{"--resume", "native title", "--", "prompt"}) {
		t.Fatalf("alias dispatch = %+v", got)
	}
}

func TestCatalogCommandIsDeterministicReadOnlyAndRejectsOtherArguments(t *testing.T) {
	var first bytes.Buffer
	if err := run(context.Background(), "agent-sessions", []string{"catalog", "--json"}, &first, defaultCommandRunners()); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := run(context.Background(), "agent-sessions", []string{"catalog", "--json"}, &second, defaultCommandRunners()); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() || !strings.HasSuffix(first.String(), "\n") || strings.HasSuffix(first.String(), "\n\n") {
		t.Fatalf("catalog output is not canonical: %q", first.String())
	}
	if err := run(context.Background(), "agent-sessions", []string{"catalog"}, &bytes.Buffer{}, defaultCommandRunners()); err == nil {
		t.Fatal("catalog accepted missing --json")
	}
}

func TestPeerLaneHookAndConnectorFailureCannotInvokeDaemonLifetime(t *testing.T) {
	for _, command := range []string{"peer", "lane", "hook", "connector"} {
		t.Run(command, func(t *testing.T) {
			daemonStarts := 0
			unavailable := errors.New("daemon unavailable")
			runners := commandRunners{daemon: func(context.Context, clihelp.Invocation, io.Writer) error {
				daemonStarts++
				return nil
			}}
			failed := func(context.Context, clihelp.Invocation, io.Writer) error { return unavailable }
			switch command {
			case "peer":
				runners.peer = failed
			case "lane":
				runners.lane = failed
			case "hook":
				runners.hook = failed
			case "connector":
				runners.connector = failed
			}
			err := run(context.Background(), "agent-sessions", []string{command, "codex"}, &bytes.Buffer{}, runners)
			if !errors.Is(err, unavailable) || daemonStarts != 0 {
				t.Fatalf("%s error=%v daemonStarts=%d", command, err, daemonStarts)
			}
		})
	}
}

func TestRunHelpAndVersionDoNotDispatch(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"version"}, {"--version"}} {
		var output bytes.Buffer
		if err := run(context.Background(), "agent-sessions", args, &output, commandRunners{}); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(output.String()) == "" {
			t.Fatalf("%v produced no output", args)
		}
	}
}

func TestDefaultWorkflowUnavailableDoesNotCreateStateOrStartDaemon(t *testing.T) {
	stateRoot := filepath.Join(shortDaemonTestRoot(t), "state")
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", stateRoot)
	t.Setenv("CODEX_PEER_CODEX_BIN", "/bin/echo")

	err := run(context.Background(), "agent-sessions", []string{"peer", "codex"}, &bytes.Buffer{}, defaultCommandRunners())
	if err == nil || !strings.Contains(err.Error(), "daemon is unavailable") {
		t.Fatalf("peer without daemon error = %v", err)
	}
	if _, statErr := os.Lstat(stateRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workflow command created state root: %v", statErr)
	}
}

func TestLaneWorkflowReturnsUsageErrorBeforeContactingDaemon(t *testing.T) {
	stateRoot := filepath.Join(shortDaemonTestRoot(t), "absent")
	t.Setenv("AGENT_SESSIONS_STATE_ROOT", stateRoot)
	err := runLaneWorkflow(context.Background(), clihelp.Invocation{
		Product: "codex", Arguments: []string{"run", "--name", "worker", "positional prompt"},
	}, io.Discard)
	if err == nil || err.Error() != "lane run reads its prompt from stdin; positional prompts are not accepted" {
		t.Fatalf("lane usage error = %v", err)
	}
	if _, statErr := os.Lstat(stateRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("usage failure contacted or created daemon state: %v", statErr)
	}
}

func TestDefaultAdminNegotiatesRunningGenerationWithoutStartingAuthority(t *testing.T) {
	stateRoot := shortDaemonTestRoot(t)
	runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})

	var output bytes.Buffer
	if err := run(context.Background(), "agent-sessions", []string{"status", "--state-root", stateRoot}, &output, defaultCommandRunners()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"service_state":"running"`) ||
		!strings.Contains(output.String(), fmt.Sprintf(`"generation":%d`, runtime.Generation())) {
		t.Fatalf("status output = %s", output.String())
	}
}

func TestDaemonLifecycleCommandsUseOnlyUserServiceManager(t *testing.T) {
	manager := &recordingServiceManager{}
	prior := currentServiceManager
	priorWait := waitForServiceReady
	currentServiceManager = func() (servicecontrol.Manager, error) { return manager, nil }
	waitForServiceReady = func(context.Context, string) error { return nil }
	t.Cleanup(func() {
		currentServiceManager = prior
		waitForServiceReady = priorWait
	})

	for _, action := range []string{"start", "stop", "restart"} {
		var output bytes.Buffer
		if err := run(context.Background(), "agent-sessions", []string{"daemon", action}, &output, defaultCommandRunners()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `"action":"`+action+`"`) || !strings.Contains(output.String(), `"completed":true`) {
			t.Fatalf("%s output = %s", action, output.String())
		}
	}
	if !reflect.DeepEqual(manager.actions, []string{"start", "stop", "restart"}) {
		t.Fatalf("service manager actions = %v", manager.actions)
	}
	if err := run(context.Background(), "agent-sessions", []string{"daemon", "stop", "--state-root", t.TempDir()}, &bytes.Buffer{}, defaultCommandRunners()); err == nil {
		t.Fatal("daemon lifecycle accepted endpoint/service options")
	}
}

func TestWaitForDaemonReadyBridgesServiceManagerSocketPublicationRace(t *testing.T) {
	stateRoot := shortDaemonTestRoot(t)
	started := make(chan *daemonpkg.Runtime, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		runtime, err := daemonpkg.StartRuntime(context.Background(), daemonpkg.RuntimeConfig{StateRoot: stateRoot})
		if err != nil {
			started <- nil
			return
		}
		started <- runtime
	}()
	if err := waitForDaemonReady(context.Background(), stateRoot); err != nil {
		t.Fatal(err)
	}
	runtime := <-started
	if runtime == nil {
		t.Fatal("test runtime failed to start")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractLaneHostKeepsNativeArgumentOrderingAndRejectsAmbiguity(t *testing.T) {
	host, arguments, err := extractLaneHost([]string{"--host", "macbook", "run", "--name", "worker", "--model", "native"})
	if err != nil {
		t.Fatal(err)
	}
	if host != "macbook" || !reflect.DeepEqual(arguments, []string{"run", "--name", "worker", "--model", "native"}) {
		t.Fatalf("remote lane wrapper = host %q args %q", host, arguments)
	}
	if _, _, err := extractLaneHost([]string{"--host=a", "--host", "b", "list"}); err == nil {
		t.Fatal("duplicate remote host selector was accepted")
	}
}

type laneInputTrap struct{ reads int }

func (reader *laneInputTrap) Read([]byte) (int, error) {
	reader.reads++
	return 0, errors.New("lane input was read unexpectedly")
}

func TestLaneInputPolicyIsSharedAcrossEveryLifecycleCommand(t *testing.T) {
	for _, command := range []string{"wait", "status", "interrupt", "archive", "list", "doctor"} {
		t.Run(command, func(t *testing.T) {
			input := &laneInputTrap{}
			body, err := readLaneInput(context.Background(), []string{command}, input)
			if err != nil || len(body) != 0 || input.reads != 0 {
				t.Fatalf("%s input body=%q reads=%d error=%v", command, body, input.reads, err)
			}
		})
	}
	for _, command := range []string{"run", "start", "resume"} {
		t.Run(command, func(t *testing.T) {
			body, err := readLaneInput(context.Background(), []string{command}, strings.NewReader("briefing"))
			if err != nil || string(body) != "briefing" {
				t.Fatalf("%s input body=%q error=%v", command, body, err)
			}
		})
	}
}

func TestLaneInputReadHonorsCancellationAndSizeLimit(t *testing.T) {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readLaneInput(ctx, []string{"start"}, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lane input error = %v", err)
	}
	_ = reader.Close()
	_ = writer.Close()

	oversized := strings.NewReader(strings.Repeat("x", maxLaneInputBytes+1))
	if _, err := readLaneInput(context.Background(), []string{"run"}, oversized); err == nil ||
		!strings.Contains(err.Error(), "exceeds 1048576 bytes") {
		t.Fatalf("oversized lane input error = %v", err)
	}
}
