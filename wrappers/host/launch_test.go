package host

import (
	"context"
	"errors"
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

func TestInteractivePlan(t *testing.T) {
	plan, err := InteractivePlan("example", []string{
		"--model", "-g", "-g", "project", "--group=review", "--", "-g", "literal",
	}, []string{"PATH=/bin", GroupsEnv + "=[\"old\"]"}, PeerIdentity{SessionID: "id", Name: "name"}, func(option string) bool { return option == "--model" })
	must(t, err)
	wantArgs := []string{"--model", "-g", "--", "-g", "literal"}
	check(t, reflect.DeepEqual(plan.Args, wantArgs), "args = %q", plan.Args)
	joined := "\n" + strings.Join(plan.Env, "\n") + "\n"
	for _, want := range []string{"\nAGENTBUS_SESSION_ID=id\n", "\nAGENTBUS_SESSION_NAME=name\n", "\nAGENTBUS_GROUPS=[\"project\",\"review\"]\n"} {
		check(t, strings.Contains(joined, want), "environment missing %q: %s", want, joined)
	}
}

func TestInteractivePlanErrors(t *testing.T) {
	for _, arguments := range [][]string{{"-g"}, {"--group="}} {
		_, err := InteractivePlan("product", arguments, nil, PeerIdentity{}, nil)
		check(t, err != nil, "arguments %q succeeded", arguments)
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
			input, err := command.StdinPipe()
			must(t, err)
			child, err := StartChild(command, lock, endpoint)
			must(t, err)
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
	input, err := command.StdinPipe()
	must(t, err)
	child, err := StartChild(command, lock, endpoint)
	must(t, err)
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
