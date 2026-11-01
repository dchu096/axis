package shadow

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/google/uuid"

	"github.com/0x7d8/wings/config"
	"github.com/0x7d8/wings/remote"
	"github.com/0x7d8/wings/server/backup"
	"github.com/0x7d8/wings/server/filesystem"
)

const (
	StateMissing  = "missing"
	StateCreating = "creating"
	StateReady    = "ready"
	StateFailed   = "failed"
)

// Manager tracks the in-memory shadow lifecycle for a single server.
type Manager struct {
	mu sync.Mutex

	serverUUID string
	client     remote.Client
	live       *filesystem.Filesystem
	log        *log.Entry

	state            string
	shadowUUID       string
	progressPercent  int
	bytes            int64
	errMessage       string
	cancel           context.CancelFunc
	createGeneration uint64
}

// Status is the JSON shape returned by GET /shadow.
type Status struct {
	State           string  `json:"state"`
	ProgressPercent *int    `json:"progress_percent,omitempty"`
	Bytes           *int64  `json:"bytes,omitempty"`
	Error           *string `json:"error,omitempty"`
}

// CreateRequest is the body for POST /shadow.
type CreateRequest struct {
	ShadowUUID    string             `json:"shadow_uuid"`
	BackupUUID    string             `json:"backup_uuid"`
	Adapter       backup.AdapterType `json:"adapter"`
	DownloadURL   string             `json:"download_url"`
	RequiredBytes int64              `json:"required_bytes"`
}

// New returns a shadow manager for the given server.
func New(serverUUID string, client remote.Client, live *filesystem.Filesystem, logger *log.Entry) *Manager {
	return &Manager{
		serverUUID: serverUUID,
		client:     client,
		live:       live,
		log:        logger,
		state:      StateMissing,
	}
}

// RootPath returns the on-disk shadow directory for this server.
func (m *Manager) RootPath() string {
	return filepath.Join(config.Get().System.ShadowDirectory, m.serverUUID)
}

// ParentPath returns the configured shadow parent directory (for Statfs).
func ParentPath() string {
	return config.Get().System.ShadowDirectory
}

// Preflight checks host free space on the shadow filesystem.
func Preflight(requiredBytes int64) (ok bool, freeBytes int64, message string) {
	free, err := FreeBytes(ParentPath())
	if err != nil {
		return false, 0, "failed to determine free disk space: " + err.Error()
	}
	if requiredBytes > 0 && free < requiredBytes {
		return false, free, fmt.Sprintf(
			"not enough free disk space on the node: need %d bytes, have %d bytes available",
			requiredBytes, free,
		)
	}
	return true, free, ""
}

// Status returns the current shadow state. If memory was cleared (daemon
// restart) but a completed tree remains on disk, report ready.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == StateMissing || m.state == "" {
		if dirExistsNonEmpty(m.RootPath()) {
			m.state = StateReady
			if m.bytes == 0 {
				if size, err := directorySize(m.RootPath()); err == nil {
					m.bytes = size
				}
			}
		} else {
			return Status{State: StateMissing}
		}
	}

	st := Status{State: m.state}
	switch m.state {
	case StateCreating:
		p := m.progressPercent
		st.ProgressPercent = &p
		b := m.bytes
		st.Bytes = &b
	case StateReady:
		b := m.bytes
		st.Bytes = &b
	case StateFailed:
		if m.errMessage != "" {
			msg := m.errMessage
			st.Error = &msg
		}
	}
	return st
}

// Create starts async materialization of a backup into the shadow directory.
// Refuses if a create job is running or a ready shadow already exists.
func (m *Manager) Create(ctx context.Context, req CreateRequest, download io.ReadCloser) error {
	if _, err := uuid.Parse(req.ShadowUUID); err != nil {
		if download != nil {
			_ = download.Close()
		}
		return errors.New("shadow: shadow_uuid must be a valid UUID")
	}
	if _, err := uuid.Parse(req.BackupUUID); err != nil {
		if download != nil {
			_ = download.Close()
		}
		return errors.New("shadow: backup_uuid must be a valid UUID")
	}

	m.mu.Lock()
	switch m.state {
	case StateCreating:
		m.mu.Unlock()
		if download != nil {
			_ = download.Close()
		}
		return errors.New("shadow: a shadow is already being created for this server")
	case StateReady:
		m.mu.Unlock()
		if download != nil {
			_ = download.Close()
		}
		return errors.New("shadow: a shadow already exists for this server; delete it first")
	}
	if dirExistsNonEmpty(m.RootPath()) && m.state != StateFailed {
		m.mu.Unlock()
		if download != nil {
			_ = download.Close()
		}
		return errors.New("shadow: a shadow directory already exists for this server; delete it first")
	}

	if ok, _, message := Preflight(req.RequiredBytes); !ok {
		m.mu.Unlock()
		if download != nil {
			_ = download.Close()
		}
		return errors.New(message)
	}

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	root := m.RootPath()
	_ = os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		m.mu.Unlock()
		if download != nil {
			_ = download.Close()
		}
		return errors.Wrap(err, "shadow: failed to create shadow directory")
	}

	jobCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.state = StateCreating
	m.shadowUUID = req.ShadowUUID
	m.progressPercent = 0
	m.bytes = 0
	m.errMessage = ""
	m.createGeneration++
	gen := m.createGeneration
	m.mu.Unlock()

	go m.runCreate(jobCtx, gen, req, download)
	return nil
}

func (m *Manager) runCreate(ctx context.Context, gen uint64, req CreateRequest, download io.ReadCloser) {
	defer func() {
		if download != nil {
			_ = download.Close()
		}
	}()

	logger := m.log.WithFields(log.Fields{
		"shadow": req.ShadowUUID,
		"backup": req.BackupUUID,
		"adapter": req.Adapter,
	})
	logger.Info("starting shadow materialization")

	var extractErr error
	defer func() {
		m.finishCreate(gen, req.ShadowUUID, extractErr)
	}()

	shadowFS, err := filesystem.New(m.RootPath(), 0, nil)
	if err != nil {
		extractErr = errors.Wrap(err, "failed to initialize shadow filesystem")
		return
	}

	progressDone := make(chan struct{})
	go m.progressLoop(ctx, gen, req.ShadowUUID, req.RequiredBytes, progressDone)
	defer close(progressDone)

	callback := func(file string, info fs.FileInfo, r io.ReadCloser) error {
		defer r.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		name := strings.TrimPrefix(filepath.ToSlash(file), "/")
		if name == "" || name == "." {
			return nil
		}
		if info.IsDir() {
			parent := filepath.ToSlash(filepath.Dir(name))
			base := filepath.Base(name)
			if parent == "." {
				parent = "/"
			}
			if err := shadowFS.CreateDirectory(base, parent); err != nil && !os.IsExist(err) {
				return err
			}
			return nil
		}

		if err := shadowFS.Write(name, r, info.Size(), info.Mode()); err != nil {
			return err
		}
		atime := info.ModTime()
		if err := shadowFS.Chtimes(name, atime, atime); err != nil {
			return err
		}

		m.mu.Lock()
		if m.createGeneration == gen {
			m.bytes += info.Size()
		}
		m.mu.Unlock()
		return nil
	}

	switch req.Adapter {
	case backup.LocalBackupAdapter:
		b, _, err := backup.LocateLocal(m.client, req.BackupUUID)
		if err != nil {
			extractErr = errors.Wrap(err, "failed to locate local backup")
			return
		}
		extractErr = b.Restore(ctx, nil, callback)
	case backup.S3BackupAdapter:
		if download == nil {
			extractErr = errors.New("S3 shadow create requires a download stream")
			return
		}
		extractErr = backup.NewS3(m.client, req.BackupUUID, "").Restore(ctx, download, callback)
	default:
		extractErr = fmt.Errorf("unsupported shadow adapter: %s", req.Adapter)
	}

	if extractErr != nil {
		if isOutOfDisk(extractErr) {
			extractErr = errors.New("out of disk")
		} else if errors.Is(extractErr, context.Canceled) {
			extractErr = errors.New("shadow create canceled")
		}
		logger.WithField("error", extractErr).Error("shadow materialization failed")
		return
	}

	size, err := directorySize(m.RootPath())
	if err != nil {
		extractErr = errors.Wrap(err, "failed to measure shadow size")
		return
	}
	m.mu.Lock()
	if m.createGeneration == gen {
		m.bytes = size
		m.progressPercent = 100
	}
	m.mu.Unlock()
	logger.WithField("bytes", size).Info("shadow materialization completed")
}

func (m *Manager) progressLoop(ctx context.Context, gen uint64, shadowUUID string, requiredBytes int64, done <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastSent := -1
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			m.mu.Lock()
			if m.createGeneration != gen || m.state != StateCreating {
				m.mu.Unlock()
				return
			}
			written := m.bytes
			pct := 0
			if requiredBytes > 0 {
				pct = int((written * 100) / requiredBytes)
				if pct > 99 {
					pct = 99
				}
				if pct < 0 {
					pct = 0
				}
			} else if written > 0 {
				pct = m.progressPercent
				if pct < 1 {
					pct = 1
				}
				if pct < 99 {
					pct++
				}
			}
			m.progressPercent = pct
			m.mu.Unlock()

			if pct == lastSent {
				continue
			}
			lastSent = pct
			m.notify(context.Background(), shadowUUID, remote.ShadowStatusRequest{
				State:           "progress",
				ProgressPercent: &pct,
			})
		}
	}
}

func (m *Manager) finishCreate(gen uint64, shadowUUID string, extractErr error) {
	m.mu.Lock()
	if m.createGeneration != gen {
		m.mu.Unlock()
		return
	}
	m.cancel = nil
	if extractErr != nil {
		m.state = StateFailed
		m.errMessage = extractErr.Error()
		root := m.RootPath()
		m.mu.Unlock()
		_ = os.RemoveAll(root)
		m.notify(context.Background(), shadowUUID, remote.ShadowStatusRequest{
			State: "failed",
			Error: extractErr.Error(),
		})
		return
	}
	m.state = StateReady
	m.progressPercent = 100
	bytes := m.bytes
	m.mu.Unlock()
	m.notify(context.Background(), shadowUUID, remote.ShadowStatusRequest{
		State: "ready",
		Bytes: &bytes,
	})
}

// Cancel aborts an in-flight create and removes the shadow directory.
func (m *Manager) Cancel() error {
	return m.destroy(true)
}

// Delete removes a ready/failed/partial shadow directory and clears state.
func (m *Manager) Delete() error {
	return m.destroy(false)
}

func (m *Manager) destroy(fromCancel bool) error {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.createGeneration++
	root := m.RootPath()
	m.state = StateMissing
	m.shadowUUID = ""
	m.progressPercent = 0
	m.bytes = 0
	m.errMessage = ""
	m.mu.Unlock()

	if err := os.RemoveAll(root); err != nil && !os.IsNotExist(err) {
		if fromCancel {
			return errors.Wrap(err, "shadow: failed to remove shadow directory after cancel")
		}
		return errors.Wrap(err, "shadow: failed to remove shadow directory")
	}
	return nil
}

// ListDirectory lists entries under the shadow tree with the same Stat JSON
// shape as the live files list-directory endpoint.
func (m *Manager) ListDirectory(directory string) ([]filesystem.Stat, error) {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	if state != StateReady && !dirExistsNonEmpty(m.RootPath()) {
		return nil, os.ErrNotExist
	}
	shadowFS, err := filesystem.New(m.RootPath(), 0, nil)
	if err != nil {
		return nil, err
	}
	return shadowFS.ListDirectory(directory)
}

// Promote copies selected relative paths from the shadow onto the live volume.
func (m *Manager) Promote(paths []string) error {
	m.mu.Lock()
	state := m.state
	root := m.RootPath()
	m.mu.Unlock()

	if state != StateReady && !dirExistsNonEmpty(root) {
		return errors.New("shadow: no ready shadow to promote from")
	}
	if len(paths) == 0 {
		return errors.New("shadow: paths must not be empty")
	}
	return PromotePaths(root, m.live, paths)
}

func (m *Manager) notify(ctx context.Context, shadowUUID string, payload remote.ShadowStatusRequest) {
	if m.client == nil || shadowUUID == "" {
		return
	}
	if err := m.client.SetShadowStatus(ctx, shadowUUID, payload); err != nil {
		m.log.WithField("error", err).WithField("shadow", shadowUUID).Warn("failed to notify panel of shadow status")
	}
}

func dirExistsNonEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func isOutOfDisk(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.ENOSPC {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space left") || strings.Contains(msg, "not enough space")
}
