package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/0x7d8/wings/config"
	"github.com/google/uuid"
)

const (
	TusResumableVersion = "1.0.0"
	defaultTusChunkSize = int64(64 << 20) // 64 MiB
)

// TusUpload tracks a destination-side TUS upload spool (outside the live volume).
type TusUpload struct {
	mu sync.Mutex

	ID         string
	ServerUUID string
	Length     int64
	Offset     int64
	Checksum   string // hex sha256 from Upload-Metadata (optional but preferred)
	SpoolPath  string
	Transfer   *Transfer
	Finalizing bool
	Completed  bool
}

// Lock locks the upload for concurrent PATCH/HEAD access.
func (u *TusUpload) Lock() { u.mu.Lock() }

// Unlock unlocks the upload.
func (u *TusUpload) Unlock() { u.mu.Unlock() }

var (
	tusUploads   = make(map[string]*TusUpload)
	tusUploadsMu sync.RWMutex
)

// SpoolDirectory returns the directory used for TUS partial archives.
func SpoolDirectory() string {
	cfg := config.Get().System.Transfers.Tus
	if cfg.Spool != "" {
		return cfg.Spool
	}
	return filepath.Join(config.Get().System.TmpDirectory, "transfers")
}

func ensureSpoolDirectory() error {
	return os.MkdirAll(SpoolDirectory(), 0o700)
}

// EffectiveChunkSize returns a positive PATCH chunk size from panel hint or config.
func EffectiveChunkSize(panelHint int64) int64 {
	if panelHint > 0 {
		return panelHint
	}
	cfg := config.Get().System.Transfers.Tus.ChunkSize
	if cfg > 0 {
		return cfg
	}
	return defaultTusChunkSize
}

// RegisterTusUpload stores an in-flight destination upload.
func RegisterTusUpload(u *TusUpload) {
	tusUploadsMu.Lock()
	defer tusUploadsMu.Unlock()
	tusUploads[u.ID] = u
}

// GetTusUpload returns a destination upload by id.
func GetTusUpload(id string) *TusUpload {
	tusUploadsMu.RLock()
	defer tusUploadsMu.RUnlock()
	return tusUploads[id]
}

// GetTusUploadByServer returns a destination upload for a server UUID, if any.
func GetTusUploadByServer(serverUUID string) *TusUpload {
	tusUploadsMu.RLock()
	defer tusUploadsMu.RUnlock()
	for _, u := range tusUploads {
		if u.ServerUUID == serverUUID {
			return u
		}
	}
	return nil
}

// RemoveTusUpload drops an upload from the registry (does not delete spool files).
func RemoveTusUpload(id string) {
	tusUploadsMu.Lock()
	defer tusUploadsMu.Unlock()
	delete(tusUploads, id)
}

// NewTusUploadID returns a new upload identifier.
func NewTusUploadID() string {
	return uuid.New().String()
}

func spoolPathFor(id string) string {
	return filepath.Join(SpoolDirectory(), id+".tar.zst")
}

// CleanupSpool removes the spool file for an upload if present.
func (u *TusUpload) CleanupSpool() {
	if u == nil || u.SpoolPath == "" {
		return
	}
	_ = os.Remove(u.SpoolPath)
}

// Abort removes the upload from the registry and deletes its spool.
func (u *TusUpload) Abort() {
	if u == nil {
		return
	}
	RemoveTusUpload(u.ID)
	u.CleanupSpool()
}

// CreateSpoolFile creates an empty spool file for the upload.
func CreateSpoolFile(id string) (string, error) {
	if err := ensureSpoolDirectory(); err != nil {
		return "", fmt.Errorf("tus: create spool dir: %w", err)
	}
	path := spoolPathFor(id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("tus: create spool file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
