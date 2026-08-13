package federator

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"testing"
	"time"
)

func TestLaneWatchHelperProcess(_ *testing.T) {
	if os.Getenv("PEER_FEDERATOR_WATCH_HELPER") == "" {
		return
	}
	if err := os.WriteFile(os.Getenv("PEER_FEDERATOR_WATCH_MARKER"), []byte("ready\n"), 0600); err != nil {
		os.Exit(2)
	}
	if os.Getenv("PEER_FEDERATOR_WATCH_HELPER") == "ignore" {
		signal.Ignore(os.Interrupt)
		for {
			time.Sleep(time.Hour)
		}
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt
	os.Exit(130)
}

func TestLaneWatchReapsChildWhenAgentLivenessCloses(t *testing.T) {
	for _, mode := range []string{"interrupt", "ignore"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ready")
			t.Setenv("PEER_FEDERATOR_WATCH_HELPER", mode)
			t.Setenv("PEER_FEDERATOR_WATCH_MARKER", marker)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			result := make(chan error, 1)
			go func() {
				_, runErr := runLaneWatch(
					context.Background(), reader,
					[]string{os.Args[0], "-test.run=TestLaneWatchHelperProcess"}, 100*time.Millisecond,
				)
				result <- runErr
			}()
			if !waitFor(func() bool { _, statErr := os.Stat(marker); return statErr == nil }, 2*time.Second) {
				t.Fatal("watchdog child did not start")
			}
			_ = writer.Close()
			select {
			case runErr := <-result:
				if runErr != nil {
					t.Fatal(runErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("watchdog did not reap child after owner death")
			}
		})
	}
}
