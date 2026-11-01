package transfer

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0x7d8/wings/remote"
)

// Panel-facing transfer stages (exact strings required by Atlas).
const (
	PanelPending    = "pending"
	PanelStopping   = "stopping"
	PanelPreparing  = "preparing"
	PanelCopying    = "copying"
	PanelVerifying  = "verifying"
	PanelFinalizing = "finalizing"
	PanelCompleted  = "completed"
	PanelFailed     = "failed"
	PanelCancelled  = "cancelled"
)

const maxProgressMessageLen = 500

// SetClient attaches the panel remote client used for progress heartbeats.
func (t *Transfer) SetClient(c remote.Client) {
	t.client = c
}

// ReportProgress posts a mid-flight heartbeat to the panel. Errors are logged
// only — they must not abort the transfer.
func (t *Transfer) ReportProgress(status, message string, bytesCopied, bytesTotal uint64) {
	if t.client == nil || t.Server == nil {
		return
	}

	req := remote.TransferProgressRequest{
		Status:      status,
		BytesCopied: bytesCopied,
		BytesTotal:  bytesTotal,
		Message:     truncateMessage(message),
		Method:      t.Method,
	}
	if bytesTotal > 0 {
		p := int((bytesCopied * 100) / bytesTotal)
		if p > 100 {
			p = 100
		}
		req.Progress = &p
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := t.client.SetTransferProgress(ctx, t.Server.ID(), req); err != nil {
		t.Log().WithError(err).WithField("status", status).Warn("failed to report transfer progress to panel")
	}
}

// StartCopyHeartbeat emits copying heartbeats on an interval until cancel is called.
// getBytes should return (copied, total); either may be zero when unknown.
func (t *Transfer) StartCopyHeartbeat(interval time.Duration, message string, getBytes func() (copied, total uint64)) (stop func()) {
	if interval <= 0 {
		interval = 20 * time.Second
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.ctx.Done():
				return
			case <-ticker.C:
				var copied, total uint64
				if getBytes != nil {
					copied, total = getBytes()
				}
				t.ReportProgress(PanelCopying, message, copied, total)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func truncateMessage(msg string) string {
	if msg == "" {
		return msg
	}
	if utf8.RuneCountInString(msg) <= maxProgressMessageLen {
		return msg
	}
	runes := []rune(msg)
	return string(runes[:maxProgressMessageLen])
}
