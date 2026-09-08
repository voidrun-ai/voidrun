package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"voidrun/model"
)

const (
	defaultConsoleLogMaxBytes int64 = 10 << 20
	defaultConsoleLogMaxFiles       = 5
)

var (
	ConsoleLogMaxBytes int64 = defaultConsoleLogMaxBytes
	ConsoleLogMaxFiles       = defaultConsoleLogMaxFiles
	consolePumps       sync.Map
)

type consolePump struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// GetConsoleFifoPath is the CLH virtio-console target; a host pump copies
// it to console.log and rotates like kubelet (10Mi x 5 by default).
func GetConsoleFifoPath(sbxID string) string {
	return fmt.Sprintf("%s/%s/console.fifo", InstancesRoot, sbxID)
}

func SetConsoleLogLimits(maxBytes int64, maxFiles int) {
	if maxBytes > 0 {
		ConsoleLogMaxBytes = maxBytes
	}
	if maxFiles > 0 {
		ConsoleLogMaxFiles = maxFiles
	}
}

func consoleLogCLI(spec model.SandboxSpec) string {
	if spec.ConsoleLogEnabled {
		return "file=" + GetConsoleFifoPath(spec.ID)
	}
	return "off"
}

func consoleLogAPI(spec model.SandboxSpec) *ConsoleConfig {
	if spec.ConsoleLogEnabled {
		return &ConsoleConfig{Mode: "File", File: GetConsoleFifoPath(spec.ID)}
	}
	return &ConsoleConfig{Mode: "Null"}
}

// prepareConsoleLog starts the host pump when enabled, or when restoring
// a VM that already has a fifo from an older always-on create.
func prepareConsoleLog(spec model.SandboxSpec) {
	if spec.ConsoleLogEnabled {
		ensureConsoleLog(spec.ID)
		return
	}
	path := GetConsoleFifoPath(spec.ID)
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		return
	}
	startConsolePump(spec.ID)
}

func ensureConsoleLog(sbxID string) {
	if err := ensureConsoleFifo(sbxID); err != nil {
		return
	}
	startConsolePump(sbxID)
}

func ensureConsoleFifo(sbxID string) error {
	if err := os.MkdirAll(GetInstanceDir(sbxID), 0o755); err != nil {
		return err
	}
	path := GetConsoleFifoPath(sbxID)
	st, err := os.Lstat(path)
	if err == nil {
		if st.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return syscall.Mkfifo(path, 0o644)
}

func startConsolePump(sbxID string) {
	fifo, err := os.OpenFile(GetConsoleFifoPath(sbxID), os.O_RDWR, 0)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &consolePump{cancel: cancel, done: make(chan struct{})}
	if _, loaded := consolePumps.LoadOrStore(sbxID, p); loaded {
		cancel()
		_ = fifo.Close()
		return
	}
	go func() {
		defer close(p.done)
		runConsolePump(ctx, sbxID, fifo)
	}()
}

func StopConsolePump(sbxID string) {
	v, ok := consolePumps.LoadAndDelete(sbxID)
	if !ok {
		return
	}
	p := v.(*consolePump)
	p.cancel()
	<-p.done
}

func runConsolePump(ctx context.Context, sbxID string, fifo *os.File) {
	defer fifo.Close()
	go func() {
		<-ctx.Done()
		_ = fifo.Close()
	}()

	buf := make([]byte, 32*1024)
	var w *rotatingWriter
	defer func() {
		if w != nil {
			_ = w.Close()
		}
	}()

	for {
		n, err := fifo.Read(buf)
		if n > 0 {
			if w == nil {
				w, err = newRotatingWriter(GetConsoleLogPath(sbxID), ConsoleLogMaxBytes, ConsoleLogMaxFiles)
				if err != nil {
					return
				}
			}
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			if ctx.Err() != nil || err == io.EOF {
				return
			}
			return
		}
	}
}

type rotatingWriter struct {
	path     string
	maxBytes int64
	maxFiles int
	f        *os.File
	size     int64
}

func newRotatingWriter(path string, maxBytes int64, maxFiles int) (*rotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = defaultConsoleLogMaxBytes
	}
	if maxFiles < 1 {
		maxFiles = 1
	}
	w := &rotatingWriter{path: path, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.size = st.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

func (w *rotatingWriter) rotate() error {
	if err := w.Close(); err != nil {
		return err
	}
	if err := rotateNumberedLogs(w.path, w.maxFiles); err != nil {
		return err
	}
	return w.open()
}

func rotateNumberedLogs(path string, maxFiles int) error {
	if maxFiles <= 1 {
		return os.Truncate(path, 0)
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", path, maxFiles-1))
	for i := maxFiles - 2; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", path, i)
		to := fmt.Sprintf("%s.%d", path, i+1)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return os.Rename(path, path+".1")
}
