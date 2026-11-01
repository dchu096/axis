package router

import (
	"fmt"
	"time"

	wserver "github.com/0x7d8/wings/server"
	"github.com/apex/log"
)

const chunkTTL = 30 * time.Minute
const uploadTmpDir = ".upload-tmp"

// StartChunkCleaner launches a background goroutine that scans every server's
// .upload-tmp directory every 5 minutes and removes session subdirectories that
// have not been modified for chunkTTL (i.e. the upload was abandoned).
func StartChunkCleaner(m *wserver.Manager) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cleanExpiredChunks(m)
		}
	}()
}

func cleanExpiredChunks(m *wserver.Manager) {
	for _, srv := range m.All() {
		fs := srv.Filesystem()
		entries, err := fs.ListDirectory(uploadTmpDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if time.Since(entry.ModTime()) > chunkTTL {
				sessionPath := fmt.Sprintf("%s/%s", uploadTmpDir, entry.Name())
				if err := fs.Delete(sessionPath); err == nil {
					log.WithField("server", srv.ID()).
						WithField("session", entry.Name()).
						Info("cleaned up orphaned upload session")
				}
			}
		}
	}
}
