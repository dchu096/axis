package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/0x7d8/wings/environment"
	"github.com/0x7d8/wings/router/middleware"
	"github.com/0x7d8/wings/server"
	"github.com/0x7d8/wings/server/installer"
	"github.com/0x7d8/wings/server/transfer"
)

// Data passed over to initiate a server transfer.
type serverTransferRequest struct {
	URL    string                  `binding:"required" json:"url"`
	Token  string                  `binding:"required" json:"token"`
	Method string                  `json:"method"`
	Tus    *serverTransferTusHint  `json:"tus"`
	Server installer.ServerDetails `json:"server"`
}

type serverTransferTusHint struct {
	Version    string   `json:"version"`
	Extensions []string `json:"extensions"`
	ChunkSize  int64    `json:"chunk_size"`
}

// postServerTransfer handles the start of a transfer for a server.
func postServerTransfer(c *gin.Context) {
	var data serverTransferRequest
	if err := c.BindJSON(&data); err != nil {
		return
	}

	s := middleware.ExtractServer(c)

	method, err := transfer.ResolveMethod(data.Method)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	// Tear down DevTools before archiving so the sidecar is not racing the transfer.
	if mgr := s.DevTools(); mgr != nil {
		_ = mgr.Destroy(c.Request.Context())
	}

	// Check if the server is already being transferred.
	if s.IsTransferring() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "A transfer is already in progress for this server.",
		})
		return
	}

	manager := middleware.ExtractManager(c)
	client := manager.Client()

	notifyPanelOfFailure := func(trnsfr *transfer.Transfer, cancelled bool, message string) {
		status := transfer.PanelFailed
		if cancelled {
			status = transfer.PanelCancelled
		}
		if trnsfr != nil {
			if message != "" {
				trnsfr.SetFailureMessage(message)
			}
			trnsfr.ReportProgress(status, status, 0, 0)
			if msg := trnsfr.FailureMessage(); msg != "" {
				message = msg
			}
			trnsfr.AbortTusUpload()
		}
		if err := client.SetTransferStatusMessage(context.Background(), s.ID(), false, message); err != nil {
			s.Log().WithField("subsystem", "transfer").
				WithField("status", false).
				WithError(err).
				Error("failed to set transfer status")
		}

		s.Events().Publish(server.TransferStatusEvent, "failure")
		s.SetTransferring(false)
	}

	// Block the server from starting while we are transferring it.
	s.SetTransferring(true)

	// Create a new transfer instance early so we can report stopping.
	trnsfr := transfer.New(context.Background(), s)
	trnsfr.SetClient(client)
	trnsfr.ReportProgress(transfer.PanelPending, "transfer accepted", 0, 0)
	trnsfr.ReportProgress(transfer.PanelStopping, "stopping server", 0, 0)

	// Ensure the server is offline. Sometimes a "No such container" error gets through
	// which means the server is already stopped. We can ignore that.
	if s.Environment.State() != environment.ProcessOfflineState {
		if err := s.Environment.WaitForStop(
			s.Context(),
			time.Second*15,
			false,
		); err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such container") {
			s.SetTransferring(false)
			trnsfr.ReportProgress(transfer.PanelFailed, "failed to stop server", 0, 0)
			middleware.CaptureAndAbort(c, fmt.Errorf("failed to stop server for transfer: %w", err))
			return
		}
	}

	transfer.Outgoing().Add(trnsfr)

	go func() {
		defer transfer.Outgoing().Remove(trnsfr)

		failedNotified := false
		defer func() {
			if !failedNotified && trnsfr.Status() != transfer.StatusCompleted {
				// Process died / unexpected exit while still transferring.
				if s.IsTransferring() {
					notifyPanelOfFailure(trnsfr, false, "transfer aborted unexpectedly")
					failedNotified = true
				}
			}
		}()

		if method == "tus" {
			chunkSize := int64(0)
			if data.Tus != nil {
				chunkSize = data.Tus.ChunkSize
			}
			err := trnsfr.PushTusToTarget(trnsfr.Context(), data.URL, data.Token, chunkSize)
			if err == nil {
				failedNotified = true // success: dest notifies panel
				trnsfr.SetStatus(transfer.StatusCompleted)
				trnsfr.Log().WithField("transfer_method", "tus").Debug("transfer complete")
				return
			}
			if errors.Is(err, context.Canceled) {
				notifyPanelOfFailure(trnsfr, true, "transfer cancelled")
				failedNotified = true
				trnsfr.Log().Debug("canceled")
				trnsfr.SendMessage("Canceled.")
				trnsfr.SetStatus(transfer.StatusCancelled)
				return
			}
			if transfer.IsTusUnsupported(err) {
				trnsfr.Log().WithError(err).Warn("TUS unavailable on destination; falling back to archive transfer")
				trnsfr.SendMessage("TUS unavailable on destination; falling back to archive...")
			} else {
				notifyPanelOfFailure(trnsfr, false, err.Error())
				failedNotified = true
				trnsfr.Log().WithError(err).Error("failed TUS transfer")
				trnsfr.SendMessage("TUS transfer failed: " + err.Error())
				trnsfr.SetStatus(transfer.StatusFailed)
				return
			}
		}

		archiveURL := data.URL
		if method == "tus" {
			archiveURL = transfer.ArchiveURLFromTusURL(data.URL)
		}

		if _, err := trnsfr.PushArchiveToTarget(archiveURL, data.Token); err != nil {
			cancelled := errors.Is(err, context.Canceled)
			msg := err.Error()
			if cancelled {
				msg = "transfer cancelled"
			}
			notifyPanelOfFailure(trnsfr, cancelled, msg)
			failedNotified = true

			if cancelled {
				trnsfr.Log().Debug("canceled")
				trnsfr.SendMessage("Canceled.")
				trnsfr.SetStatus(transfer.StatusCancelled)
				return
			}

			trnsfr.Log().WithError(err).Error("failed to push archive to target")
			trnsfr.SetStatus(transfer.StatusFailed)
			return
		}

		// DO NOT NOTIFY THE PANEL OF SUCCESS HERE. The only node that should send
		// a success status is the destination node.
		failedNotified = true
		trnsfr.SetStatus(transfer.StatusCompleted)
		trnsfr.Log().WithField("transfer_method", "archive").Debug("transfer complete")
	}()

	c.Status(http.StatusAccepted)
}

// deleteServerTransfer cancels an outgoing transfer for a server.
func deleteServerTransfer(c *gin.Context) {
	s := middleware.ExtractServer(c)

	trnsfr := transfer.Outgoing().Get(s.ID())
	if !s.IsTransferring() || trnsfr == nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr.ReportProgress(transfer.PanelCancelled, "cancel requested", 0, 0)
	trnsfr.AbortTusUpload()
	trnsfr.Cancel()

	c.Status(http.StatusAccepted)
}
