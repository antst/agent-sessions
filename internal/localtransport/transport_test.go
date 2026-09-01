package localtransport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrameRoundTripAndBounds(t *testing.T) {
	limits := Limits{MaxFrameBytes: 128, MaxNesting: 4, MaxStringBytes: 16}
	body := []byte(`{"message":"hello","nested":{"ok":true}}`)
	var wire bytes.Buffer
	if err := WriteFrame(&wire, body, limits); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&wire, limits)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("ReadFrame() = %s, want %s", got, body)
	}

	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "empty", body: nil},
		{name: "oversized", body: []byte(`{"value":"` + strings.Repeat("x", 129) + `"}`)},
		{name: "not-object", body: []byte(`[]`)},
		{name: "invalid-utf8", body: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}},
		{name: "deep", body: []byte(`{"a":{"b":{"c":{"d":{"e":1}}}}}`)},
		{name: "long-string", body: []byte(`{"value":"` + strings.Repeat("x", 17) + `"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := WriteFrame(&output, test.body, limits); err == nil {
				t.Fatalf("WriteFrame accepted %q", test.body)
			}
		})
	}
}

func TestReadFrameRejectsLengthBeforeAllocationAndTruncation(t *testing.T) {
	limits := Limits{MaxFrameBytes: 32, MaxNesting: 4, MaxStringBytes: 16}
	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], 33)
	reader := &countingReader{Reader: bytes.NewReader(append(oversized[:], bytes.Repeat([]byte("x"), 33)...))}
	if _, err := ReadFrame(reader, limits); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("oversized ReadFrame error = %v", err)
	}
	if reader.n != 4 {
		t.Fatalf("oversized frame consumed %d bytes, want header only", reader.n)
	}

	var truncated bytes.Buffer
	binary.BigEndian.PutUint32(oversized[:], 12)
	truncated.Write(oversized[:])
	truncated.WriteString(`{"x":`)
	if _, err := ReadFrame(&truncated, limits); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated ReadFrame error = %v", err)
	}
}

func TestLimitsFailClosed(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil), Limits{}); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("zero limits error = %v", err)
	}
}

func TestListenRejectsNonPrivateAndExistingSocketPaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "component.sock")
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, DefaultLimits()); err == nil {
		t.Fatal("listener accepted a non-private parent")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", path); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, DefaultLimits()); err == nil {
		t.Fatal("listener replaced an existing symlink")
	}
}

func TestListenRejectsSymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Listen(filepath.Join(alias, "component.sock"), DefaultLimits()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Listen through ancestor symlink error = %v", err)
	}
}

func TestDialRejectsParentReplacedWithSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "runtime")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "component.sock")
	if _, err := os.Lstat(parent); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "runtime-moved")
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, parent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Dial(path, DefaultLimits()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Dial after ancestor swap error = %v", err)
	}
}

type countingReader struct {
	io.Reader
	n int
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, '{', '}'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	limits := Limits{MaxFrameBytes: 1024, MaxNesting: 16, MaxStringBytes: 256}
	f.Fuzz(func(t *testing.T, wire []byte) {
		body, err := ReadFrame(bytes.NewReader(wire), limits)
		if err != nil {
			return
		}
		if len(body) == 0 || len(body) > int(limits.MaxFrameBytes) {
			t.Fatalf("accepted body length %d", len(body))
		}
	})
}

func (r *countingReader) Read(body []byte) (int, error) {
	n, err := r.Reader.Read(body)
	r.n += n
	return n, err
}
