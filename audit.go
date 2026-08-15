package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// auditWriter appends one JSON line per recorded request to a file so that
// usage history survives gateway restarts. The file is opened lazily and
// appended without rewriting, keeping the hot path cheap.
type auditWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	buf  *bufio.Writer
}

func newAuditWriter(path string) *auditWriter {
	return &auditWriter{path: path}
}

func (a *auditWriter) write(entry map[string]any) {
	if a == nil || a.path == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureOpen(); err != nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = a.buf.Write(data)
	_ = a.buf.WriteByte('\n')
	if err := a.buf.Flush(); err != nil {
		a.close()
	}
}

func (a *auditWriter) ensureOpen() error {
	if a.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	a.f = f
	a.buf = bufio.NewWriter(f)
	return nil
}

func (a *auditWriter) close() {
	if a.f != nil {
		_ = a.f.Close()
		a.f = nil
		a.buf = nil
	}
}
