package host

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLaneModeUsesTokenPresence(t *testing.T) {
	t.Setenv(TokenEnv, "")
	check(t, LaneMode(), "empty present token did not select lane mode")
	must(t, os.Unsetenv(TokenEnv))
	check(t, !LaneMode(), "absent token selected lane mode")
}

func TestLaunchTokenDigestLength(t *testing.T) {
	got := LaunchTokenDigest("secret launch token")
	check(t, len(got) == 32, "digest length = %d, want 32", len(got))
	check(t, got == LaunchTokenDigest("secret launch token"), "digest is not stable")
	check(t, got != "secret launch token", "digest exposed launch token")
}

func TestInteractivePlan(t *testing.T) {
	plan, err := InteractivePlan("example", []string{
		"--model", "-g", "-g", "project", "--group=review", "--peer-name", "chosen", "--", "-g", "literal",
	}, []string{"PATH=/bin", GroupsEnv + "=[\"old\"]"}, PeerIdentity{SessionID: "id", Name: "name"}, func(option string) bool { return option == "--model" })
	must(t, err)
	wantArgs := []string{"--model", "-g", "--", "-g", "literal"}
	check(t, reflect.DeepEqual(plan.Args, wantArgs), "args = %q", plan.Args)
	joined := "\n" + strings.Join(plan.Env, "\n") + "\n"
	for _, want := range []string{"\nAGENTBUS_SESSION_ID=id\n", "\nAGENTBUS_SESSION_NAME=chosen\n", "\nAGENTBUS_GROUPS=[\"project\",\"review\"]\n"} {
		check(t, strings.Contains(joined, want), "environment missing %q: %s", want, joined)
	}
}

func TestInteractivePlanErrors(t *testing.T) {
	for _, arguments := range [][]string{{"-g"}, {"--group="}, {"-n"}, {"--peer-name="}} {
		_, err := InteractivePlan("product", arguments, nil, PeerIdentity{}, nil)
		check(t, err != nil, "arguments %q succeeded", arguments)
	}
}

func TestClassifiedInteractivePlanDefersProjectionErrors(t *testing.T) {
	for _, args := range [][]string{{"exec", "-g"}, {"--help", "--peer-name="}} {
		environment := []string{"PATH=/bin"}
		plan, passthrough, err := ClassifiedInteractivePlan("product", args, environment, PeerIdentity{}, nil, func(argument string) bool { return argument == "exec" || argument == "--help" })
		must(t, err)
		check(t, passthrough && reflect.DeepEqual(plan.Args, args) && reflect.DeepEqual(plan.Env, environment), "passthrough = %#v, %v", plan, passthrough)
	}
}

func TestBuildArgumentsTable(t *testing.T) {
	rules := []ArgumentRule{
		{Name: "--agent", TakesValue: true},
		{Name: "--model", TakesValue: true, ConflictField: "model"},
		{Name: "-c", TakesValue: true, Conflict: func(value string) string {
			if strings.HasPrefix(value, "model=") {
				return "model"
			}
			return ""
		}},
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"ordered", []string{"--agent", "one", "--agent=two", "-c", "web=true"}, ""},
		{"unknown", []string{"--other"}, "unsupported argument --other"},
		{"missing", []string{"--agent"}, "--agent requires a value"},
		{"typed", []string{"--model", "x"}, "argument conflicts with typed field model"},
		{"dynamic typed", []string{"-c=model=x"}, "argument conflicts with typed field model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := BuildArguments(test.args, rules)
			check(t, test.want != "" || err == nil && reflect.DeepEqual(got, test.args), "got %q, %v", got, err)
			check(t, test.want == "" || err != nil && err.Error() == test.want, "error = %v", err)
		})
	}
}

func TestChildOwnershipTable(t *testing.T) {
	for _, ownerDies := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordered close", true: "stray child holds lock"}[ownerDies], func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "bus.sock")
			lock, err := AcquireSessionLock(socket, "example", "session")
			must(t, err)
			endpoint, err := ListenPrivate(socket, "session")
			must(t, err)
			command := exec.Command(os.Args[0], "-test.run=TestOwnedChildHelper")
			command.Env = append(os.Environ(), "HOST_CHILD=1", SocketEnv+"=secret", LocalKeyEnv+"=secret", TokenEnv+"=secret", SessionIDEnv+"=secret", NameEnv+"=secret", GroupsEnv+"=secret")
			child, input, output, err := StartChild(command, lock, endpoint)
			must(t, err)
			defer output.Close()
			_, err = AcquireSessionLock(socket, "example", "session")
			check(t, err != nil && err.Error() == "session busy", "live child lock = %v", err)
			if ownerDies {
				h := &Handoff{}
				_, _ = h.Deliver(context.Background(), delivery("queued"), nil)
				must(t, lock.Close())
				_, err = AcquireSessionLock(socket, "example", "session")
				check(t, err != nil && err.Error() == "session busy", "inherited lock = %v", err)
				must(t, input.Close())
				must(t, child.Wait())
				check(t, len(h.queue) == 1, "child death changed FIFO: %d", len(h.queue))
				must(t, endpoint.Close())
			} else {
				must(t, child.Close(context.Background(), func(context.Context) error { return input.Close() }))
				_, err = os.Stat(endpoint.Path)
				check(t, os.IsNotExist(err), "endpoint remains: %v", err)
			}
			again, err := AcquireSessionLock(socket, "example", "session")
			must(t, err)
			must(t, again.Close())
		})
	}
}

func TestOwnedChildHelper(t *testing.T) {
	if os.Getenv("HOST_CHILD") != "1" {
		return
	}
	for _, name := range []string{SocketEnv, LocalKeyEnv, TokenEnv, SessionIDEnv, NameEnv, GroupsEnv} {
		if os.Getenv(name) != "" {
			os.Exit(2)
		}
	}
	if os.Getenv("HOST_CHILD_OUTPUT") != "" {
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 300000))
		os.Exit(0)
	}
	_, _ = os.Stdin.Read(make([]byte, 1))
	if os.Getenv("HOST_CHILD_FAILURE") == "1" {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestChildCloseIgnoresStoppedProcessNoise(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	lock, err := AcquireSessionLock(socket, "example", "session")
	must(t, err)
	endpoint, err := ListenPrivate(socket, "session")
	must(t, err)
	command := exec.Command(os.Args[0], "-test.run=TestOwnedChildHelper")
	command.Env = append(os.Environ(), "HOST_CHILD=1", "HOST_CHILD_FAILURE=1")
	child, input, output, err := StartChild(command, lock, endpoint)
	must(t, err)
	defer output.Close()
	err = child.Close(context.Background(), func(context.Context) error {
		_ = input.Close()
		return errors.New("stdin already closed")
	})
	must(t, err)
	_, err = os.Stat(endpoint.Path)
	check(t, os.IsNotExist(err), "endpoint remains: %v", err)
	again, err := AcquireSessionLock(socket, "example", "session")
	must(t, err)
	must(t, again.Close())
}

func TestChildOutputDrainsAfterExit(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bus.sock")
	lock, err := AcquireSessionLock(socket, "example", "session")
	must(t, err)
	endpoint, err := ListenPrivate(socket, "session")
	must(t, err)
	command := exec.Command(os.Args[0], "-test.run=TestOwnedChildHelper")
	command.Env = append(os.Environ(), "HOST_CHILD=1", "HOST_CHILD_OUTPUT=1")
	child, input, output, err := StartChild(command, lock, endpoint)
	must(t, err)
	defer input.Close()
	result := make(chan []byte, 1)
	go func() {
		body, _ := io.ReadAll(output)
		_ = output.Close()
		result <- body
	}()
	must(t, child.Wait())
	body := <-result
	check(t, len(body) == 300000, "output bytes = %d", len(body))
	must(t, child.Close(context.Background(), func(context.Context) error { return nil }))
}
