package structuredprocess

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestTailReaderKeepsBufferedLineAfterParentExit(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	start := make(chan struct{})
	tail := make(chan []byte, 1)
	go func() {
		<-start
		readTail(reader, tail)
	}()
	parent := exec.Command("sh", "-c", "printf 'written before exit\\n' >&3")
	parent.ExtraFiles = []*os.File{writer}
	if err = parent.Run(); err != nil {
		t.Fatal(err)
	}
	if err = reader.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	close(start)
	if got := string(<-tail); !strings.Contains(got, "written before exit") {
		t.Fatalf("tail = %q", got)
	}
}
