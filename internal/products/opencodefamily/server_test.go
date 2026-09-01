package opencodefamily

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/antst/agent-sessions/internal/productruntime"
)

func TestLiveServerBuildsOnlyFullKiloAttachAndKeepsAuthTransient(t *testing.T) {
	server := &LiveServer{
		endpoint: "http://127.0.0.1:43123", username: "kilo-test",
		password: productruntime.NewSensitiveValue("never-persist-this-password"),
	}
	command, err := server.BuildKiloAttach("/work/project", "ses_resume_exact", []string{"--model", "kilo/free"}, []productruntime.EnvVar{{Name: "TERM", Value: "xterm"}})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"attach", "http://127.0.0.1:43123", "--dir", "/work/project", "--session", "ses_resume_exact", "--model", "kilo/free"}
	if command.Path != "kilo" || !reflect.DeepEqual(command.Args, wantArgs) || command.Cwd != "/work/project" {
		t.Fatalf("full attach command = %#v", command)
	}
	if len(command.SensitiveEnv) != 1 || command.SensitiveEnv[0].Name != "KILO_SERVER_PASSWORD" ||
		command.SensitiveEnv[0].Value.Reveal() != "never-persist-this-password" {
		t.Fatalf("attach auth = %#v", command.SensitiveEnv)
	}
	if _, err := json.Marshal(server); err == nil {
		t.Fatal("live server containing endpoint and credential serialized")
	}
	if _, err := json.Marshal(command); err == nil {
		t.Fatal("transient attach command serialized")
	}
	for _, arguments := range [][]string{
		{"--mini"}, {"--mini=true"}, {"--session", "ses_other"}, {"--continue"}, {"-c"}, {"--fork"}, {"--cloud-fork"},
		{"--dir=/other"}, {"--port", "9"}, {"--password", "forged"}, {"-p=forged"}, {"--username", "forged"}, {"-u=forged"},
		{"--replay"}, {"--replay=false"}, {"--no-replay"}, {"--replay-limit", "10"}, {"serve"},
	} {
		if _, err := server.BuildKiloAttach("/work/project", "ses_resume_exact", arguments, nil); err == nil {
			t.Fatalf("topology override %v was accepted", arguments)
		}
	}
}

func TestLiveServerConcurrentCloseStopsExactOwnerOnce(t *testing.T) {
	var calls atomic.Int64
	server := &LiveServer{client: &Client{}, closeFn: func(context.Context) error {
		calls.Add(1)
		return nil
	}}
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := server.Close(context.Background()); err != nil {
				t.Errorf("Close() = %v", err)
			}
		}()
	}
	wait.Wait()
	if calls.Load() != 1 || server.Client() != nil {
		t.Fatalf("close calls=%d client=%p", calls.Load(), server.Client())
	}
}
