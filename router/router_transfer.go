package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/0x7d8/wings/router/middleware"
	"github.com/0x7d8/wings/router/tokens"
	"github.com/0x7d8/wings/server"
	"github.com/0x7d8/wings/server/installer"
	"github.com/0x7d8/wings/server/transfer"
)

// parseTransferToken validates the Bearer JWT used for node↔node transfer routes.
func parseTransferToken(c *gin.Context) (*tokens.TransferPayload, error) {
	auth := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(auth) != 2 || auth[0] != "Bearer" {
		c.Header("WWW-Authenticate", "Bearer")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "The required authorization heads were not present in the request.",
		})
		return nil, errors.New("unauthorized")
	}

	token := tokens.TransferPayload{}
	if err := tokens.ParseToken([]byte(auth[1]), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return nil, err
	}

	if !token.HasScope(tokens.ServerTransfer) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Forbidden."})
		return nil, errors.New("forbidden")
	}
	return &token, nil
}

// beginIncomingTransfer creates (or returns) the incoming transfer + server for a JWT subject.
func beginIncomingTransfer(c *gin.Context, subject string) (*transfer.Transfer, context.Context, context.CancelFunc, error) {
	manager := middleware.ExtractManager(c)
	u, err := uuid.Parse(subject)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return nil, nil, nil, err
	}

	trnsfr := transfer.Incoming().Get(u.String())
	if trnsfr != nil {
		ctx, cancel := context.WithCancel(trnsfr.Context())
		return trnsfr, ctx, cancel, nil
	}

	trnsfr = transfer.New(context.Background(), nil)
	trnsfr.SetClient(manager.Client())

	ctx, cancel := context.WithCancel(trnsfr.Context())

	i, err := installer.New(ctx, manager, installer.ServerDetails{
		UUID:              u.String(),
		StartOnCompletion: false,
	})
	if err != nil {
		cancel()
		msg := err.Error()
		if err2 := manager.Client().SetTransferStatusMessage(context.Background(), u.String(), false, msg); err2 != nil {
			log.WithField("status", false).WithError(err2).Error("failed to set transfer status")
		}
		middleware.CaptureAndAbort(c, err)
		return nil, nil, nil, err
	}

	i.Server().SetTransferring(true)
	manager.Add(i.Server())

	trnsfr.Server = i.Server()
	transfer.Incoming().Add(trnsfr)
	trnsfr.ReportProgress(transfer.PanelPending, "incoming transfer accepted", 0, 0)
	return trnsfr, ctx, cancel, nil
}

// abortIncomingLocal cleans up a prepared incoming transfer without notifying the panel.
func abortIncomingLocal(manager *server.Manager, trnsfr *transfer.Transfer) {
	if trnsfr == nil {
		return
	}
	if trnsfr.Server != nil {
		if u := transfer.GetTusUploadByServer(trnsfr.Server.ID()); u != nil {
			u.Abort()
		}
	}
	transfer.Incoming().Remove(trnsfr)
	trnsfr.Cancel()
	if trnsfr.Server == nil {
		return
	}
	id := trnsfr.Server.ID()
	path := trnsfr.Server.Filesystem().Path()
	manager.Remove(func(match *server.Server) bool {
		return match.ID() == id
	})
	trnsfr.Server.SetTransferring(false)
	go func() {
		if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
			log.WithError(err).WithField("path", path).Warn("failed to delete local server files after aborted transfer")
		}
	}()
}

func finalizeIncomingTransfer(manager *server.Manager, trnsfr *transfer.Transfer, successful *bool) {
	transfer.Incoming().Remove(trnsfr)

	ok := successful != nil && *successful
	if !ok {
		msg := trnsfr.FailureMessage()
		if msg == "" {
			msg = "incoming transfer failed"
		}
		trnsfr.ReportProgress(transfer.PanelFailed, msg, 0, 0)
		if trnsfr.Server != nil {
			trnsfr.Server.Events().Publish(server.TransferStatusEvent, "failure")
			manager.Remove(func(match *server.Server) bool {
				return match.ID() == trnsfr.Server.ID()
			})
		}
	}

	id := ""
	if trnsfr.Server != nil {
		id = trnsfr.Server.ID()
	}
	if id == "" {
		return
	}

	msg := ""
	if !ok {
		msg = trnsfr.FailureMessage()
	}
	if err := manager.Client().SetTransferStatusMessage(context.Background(), id, ok, msg); err != nil {
		if !ok {
			go func(trnsfr *transfer.Transfer) {
				if trnsfr.Server == nil {
					return
				}
				_ = trnsfr.Server.Filesystem().UnixFS().Close()
				if err := os.RemoveAll(trnsfr.Server.Filesystem().Path()); err != nil && !os.IsNotExist(err) {
					trnsfr.Log().WithError(err).Warn("failed to delete local server files")
				}
			}(trnsfr)
		}
		trnsfr.Log().WithField("status", ok).WithError(err).Error("failed to set transfer status on panel")
		return
	}

	if trnsfr.Server != nil {
		trnsfr.Server.SetTransferring(false)
		if ok {
			trnsfr.Server.Events().Publish(server.TransferStatusEvent, "success")
		} else {
			go func(trnsfr *transfer.Transfer) {
				_ = trnsfr.Server.Filesystem().UnixFS().Close()
				if err := os.RemoveAll(trnsfr.Server.Filesystem().Path()); err != nil && !os.IsNotExist(err) {
					trnsfr.Log().WithError(err).Warn("failed to delete local server files")
				}
			}(trnsfr)
		}
	}
}

// postTransfers receives an archive multipart transfer (fallback path).
func postTransfers(c *gin.Context) {
	token, err := parseTransferToken(c)
	if err != nil {
		return
	}

	manager := middleware.ExtractManager(c)
	trnsfr, ctx, cancel, err := beginIncomingTransfer(c, token.Subject)
	if err != nil {
		return
	}
	defer cancel()

	trnsfr.Method = "archive"
	trnsfr.SetClient(manager.Client())

	successful := false
	defer finalizeIncomingTransfer(manager, trnsfr, &successful)

	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		trnsfr.SetFailureMessage(err.Error())
		trnsfr.Log().Debug("failed to parse content type header")
		middleware.CaptureAndAbort(c, err)
		return
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		err := fmt.Errorf("invalid content type \"%s\", expected \"multipart/form-data\"", mediaType)
		trnsfr.SetFailureMessage(err.Error())
		trnsfr.Log().Debug("invalid content type")
		middleware.CaptureAndAbort(c, err)
		return
	}

	trnsfr.ReportProgress(transfer.PanelCopying, "receiving archive", 0, 0)
	stopHB := trnsfr.StartCopyHeartbeat(20*time.Second, "receiving archive", nil)
	defer stopHB()

	// Used to calculate the hash of the file as it is being uploaded.
	h := sha256.New()

	// Used to read the file and checksum from the request body.
	mr := multipart.NewReader(c.Request.Body, params["boundary"])

	// Loop through the parts of the request body and process them.
	var (
		hasArchive       bool
		hasChecksum      bool
		checksumVerified bool
	)
out:
	for {
		select {
		case <-ctx.Done():
			break out
		default:
			p, err := mr.NextPart()
			if err == io.EOF {
				break out
			}
			if err != nil {
				trnsfr.SetFailureMessage(err.Error())
				middleware.CaptureAndAbort(c, err)
				return
			}

			name := p.FormName()
			switch name {
			case "archive":
				trnsfr.Log().Debug("received archive")
				trnsfr.ReportProgress(transfer.PanelCopying, "extracting archive", 0, 0)

				if err := trnsfr.Server.EnsureDataDirectoryExists(); err != nil {
					trnsfr.SetFailureMessage(err.Error())
					middleware.CaptureAndAbort(c, err)
					return
				}

				tee := io.TeeReader(p, h)
				if err := trnsfr.Server.Filesystem().ExtractStreamUnsafe(ctx, "/", tee); err != nil {
					trnsfr.SetFailureMessage(err.Error())
					middleware.CaptureAndAbort(c, err)
					return
				}

				hasArchive = true
			case "checksum":
				trnsfr.Log().Debug("received checksum")
				trnsfr.ReportProgress(transfer.PanelVerifying, "verifying archive checksum", 0, 0)

				if !hasArchive {
					err := errors.New("archive must be sent before the checksum")
					trnsfr.SetFailureMessage(err.Error())
					middleware.CaptureAndAbort(c, err)
					return
				}

				hasChecksum = true

				v, err := io.ReadAll(p)
				if err != nil {
					trnsfr.SetFailureMessage(err.Error())
					middleware.CaptureAndAbort(c, err)
					return
				}

				expected := make([]byte, hex.DecodedLen(len(v)))
				n, err := hex.Decode(expected, v)
				if err != nil {
					trnsfr.SetFailureMessage(err.Error())
					middleware.CaptureAndAbort(c, err)
					return
				}
				actual := h.Sum(nil)

				trnsfr.Log().WithFields(log.Fields{
					"expected": hex.EncodeToString(expected),
					"actual":   hex.EncodeToString(actual),
				}).Debug("checksums")

				if !bytes.Equal(expected[:n], actual) {
					err := errors.New("checksums don't match")
					trnsfr.SetFailureMessage(err.Error())
					middleware.CaptureAndAbort(c, err)
					return
				}

				trnsfr.Log().Debug("checksums match")
				checksumVerified = true
			default:
				continue
			}
		}
	}

	if !hasArchive || !hasChecksum {
		err := errors.New("missing archive or checksum")
		trnsfr.SetFailureMessage(err.Error())
		middleware.CaptureAndAbort(c, err)
		return
	}

	if !checksumVerified {
		err := errors.New("checksums don't match")
		trnsfr.SetFailureMessage(err.Error())
		middleware.CaptureAndAbort(c, err)
		return
	}

	trnsfr.ReportProgress(transfer.PanelFinalizing, "creating environment", 0, 0)

	// Ensure the server environment gets configured.
	if err := trnsfr.Server.CreateEnvironment(); err != nil {
		trnsfr.SetFailureMessage(err.Error())
		middleware.CaptureAndAbort(c, err)
		return
	}

	successful = true
	trnsfr.ReportProgress(transfer.PanelCompleted, "archive transfer complete", 0, 0)
	trnsfr.Log().WithField("transfer_method", "archive").Debug("done!")
}

// deleteTransfer cancels an incoming transfer for a server.
func deleteTransfer(c *gin.Context) {
	s := middleware.ExtractServer(c)

	if !s.IsTransferring() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr := transfer.Incoming().Get(s.ID())
	if trnsfr == nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr.ReportProgress(transfer.PanelCancelled, "incoming cancel requested", 0, 0)
	trnsfr.SetFailureMessage("transfer cancelled")
	trnsfr.Cancel()

	// Archive transfers finalize in the POST /api/transfers handler defer when the
	// request context cancels. TUS uploads sit between PATCH calls, so cancel must
	// clean them up here.
	if trnsfr.Method == "tus" {
		if u := transfer.GetTusUploadByServer(s.ID()); u != nil {
			u.Abort()
		}
		successful := false
		finalizeIncomingTransfer(middleware.ExtractManager(c), trnsfr, &successful)
	}

	c.Status(http.StatusAccepted)
}

// --- TUS 1.0 receive ---

// postTusTransferCreate handles POST /api/transfers/tus
func postTusTransferCreate(c *gin.Context) {
	token, err := parseTransferToken(c)
	if err != nil {
		return
	}

	if v := c.GetHeader("Tus-Resumable"); v != "" && v != transfer.TusResumableVersion {
		c.Header("Tus-Resumable", transfer.TusResumableVersion)
		c.AbortWithStatusJSON(http.StatusPreconditionFailed, gin.H{
			"error": "unsupported Tus-Resumable version",
		})
		return
	}

	lengthStr := c.GetHeader("Upload-Length")
	if lengthStr == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Upload-Length required"})
		return
	}
	length, err := strconv.ParseInt(lengthStr, 10, 64)
	if err != nil || length < 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid Upload-Length"})
		return
	}

	meta := parseTusMetadata(c.GetHeader("Upload-Metadata"))
	serverUUID := meta["server_uuid"]
	if serverUUID == "" {
		serverUUID = token.Subject
	}
	if !strings.EqualFold(serverUUID, token.Subject) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "server_uuid metadata does not match token subject",
		})
		return
	}

	manager := middleware.ExtractManager(c)
	trnsfr, _, cancel, err := beginIncomingTransfer(c, token.Subject)
	if err != nil {
		return
	}
	defer cancel()

	trnsfr.Method = "tus"
	trnsfr.SetClient(manager.Client())
	trnsfr.ReportProgress(transfer.PanelPreparing, "TUS upload created", 0, uint64(length))

	id := transfer.NewTusUploadID()
	spoolPath, err := transfer.CreateSpoolFile(id)
	if err != nil {
		trnsfr.SetFailureMessage(err.Error())
		abortIncomingLocal(manager, trnsfr)
		middleware.CaptureAndAbort(c, err)
		return
	}

	upload := &transfer.TusUpload{
		ID:         id,
		ServerUUID: token.Subject,
		Length:     length,
		Offset:     0,
		Checksum:   meta["sha256"],
		SpoolPath:  spoolPath,
		Transfer:   trnsfr,
	}
	transfer.RegisterTusUpload(upload)

	c.Header("Tus-Resumable", transfer.TusResumableVersion)
	c.Header("Location", "/api/transfers/tus/"+id)
	c.Header("Upload-Offset", "0")
	c.Status(http.StatusCreated)
}

// headTusTransfer handles HEAD /api/transfers/tus/:id
func headTusTransfer(c *gin.Context) {
	token, err := parseTransferToken(c)
	if err != nil {
		return
	}

	upload := transfer.GetTusUpload(c.Param("id"))
	if upload == nil || !strings.EqualFold(upload.ServerUUID, token.Subject) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	upload.Lock()
	offset := upload.Offset
	length := upload.Length
	upload.Unlock()

	c.Header("Tus-Resumable", transfer.TusResumableVersion)
	c.Header("Upload-Offset", strconv.FormatInt(offset, 10))
	c.Header("Upload-Length", strconv.FormatInt(length, 10))
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
}

// patchTusTransfer handles PATCH /api/transfers/tus/:id
func patchTusTransfer(c *gin.Context) {
	token, err := parseTransferToken(c)
	if err != nil {
		return
	}

	upload := transfer.GetTusUpload(c.Param("id"))
	if upload == nil || !strings.EqualFold(upload.ServerUUID, token.Subject) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if upload.Transfer == nil {
		c.AbortWithStatus(http.StatusConflict)
		return
	}

	if ct := c.GetHeader("Content-Type"); ct != "" && ct != "application/offset+octet-stream" {
		c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{
			"error": "Content-Type must be application/offset+octet-stream",
		})
		return
	}

	offsetHdr := c.GetHeader("Upload-Offset")
	reqOffset, err := strconv.ParseInt(offsetHdr, 10, 64)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid Upload-Offset"})
		return
	}

	upload.Lock()
	defer upload.Unlock()

	if upload.Completed || upload.Finalizing {
		c.Header("Tus-Resumable", transfer.TusResumableVersion)
		c.Header("Upload-Offset", strconv.FormatInt(upload.Offset, 10))
		c.Status(http.StatusNoContent)
		return
	}

	if reqOffset != upload.Offset {
		c.Header("Tus-Resumable", transfer.TusResumableVersion)
		c.Header("Upload-Offset", strconv.FormatInt(upload.Offset, 10))
		c.AbortWithStatus(http.StatusConflict)
		return
	}

	f, err := os.OpenFile(upload.SpoolPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		upload.Transfer.SetFailureMessage(err.Error())
		middleware.CaptureAndAbort(c, err)
		return
	}

	n, err := io.Copy(f, c.Request.Body)
	closeErr := f.Close()
	if err != nil {
		upload.Transfer.SetFailureMessage(err.Error())
		middleware.CaptureAndAbort(c, err)
		return
	}
	if closeErr != nil {
		upload.Transfer.SetFailureMessage(closeErr.Error())
		middleware.CaptureAndAbort(c, closeErr)
		return
	}

	upload.Offset += n
	upload.Transfer.ReportProgress(
		transfer.PanelCopying,
		"receiving TUS upload",
		uint64(upload.Offset),
		uint64(upload.Length),
	)

	c.Header("Tus-Resumable", transfer.TusResumableVersion)
	c.Header("Upload-Offset", strconv.FormatInt(upload.Offset, 10))

	if upload.Offset < upload.Length {
		c.Status(http.StatusNoContent)
		return
	}

	if upload.Offset > upload.Length {
		err := fmt.Errorf("tus: upload exceeded declared length (%d > %d)", upload.Offset, upload.Length)
		upload.Transfer.SetFailureMessage(err.Error())
		failTusUpload(middleware.ExtractManager(c), upload, err)
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Complete: verify, extract, finalize.
	upload.Finalizing = true
	manager := middleware.ExtractManager(c)
	if err := completeTusUpload(c.Request.Context(), manager, upload); err != nil {
		upload.Transfer.SetFailureMessage(err.Error())
		failTusUpload(manager, upload, err)
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// deleteTusTransfer handles DELETE /api/transfers/tus/:id (termination extension).
func deleteTusTransfer(c *gin.Context) {
	token, err := parseTransferToken(c)
	if err != nil {
		return
	}

	upload := transfer.GetTusUpload(c.Param("id"))
	if upload == nil || !strings.EqualFold(upload.ServerUUID, token.Subject) {
		c.Status(http.StatusNoContent)
		return
	}

	manager := middleware.ExtractManager(c)
	if upload.Transfer != nil {
		upload.Transfer.SetFailureMessage("TUS upload terminated")
		abortIncomingLocal(manager, upload.Transfer)
	} else {
		upload.Abort()
	}

	c.Header("Tus-Resumable", transfer.TusResumableVersion)
	c.Status(http.StatusNoContent)
}

func completeTusUpload(ctx context.Context, manager *server.Manager, upload *transfer.TusUpload) error {
	trnsfr := upload.Transfer
	trnsfr.ReportProgress(transfer.PanelVerifying, "verifying TUS archive checksum", uint64(upload.Length), uint64(upload.Length))

	if upload.Checksum != "" {
		sum, err := hashFileSHA256(upload.SpoolPath)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, upload.Checksum) {
			return fmt.Errorf("checksums don't match: expected %s got %s", upload.Checksum, sum)
		}
		trnsfr.Log().Debug("TUS spool checksum matches")
	}

	trnsfr.ReportProgress(transfer.PanelCopying, "extracting TUS archive", uint64(upload.Length), uint64(upload.Length))
	if err := trnsfr.Server.EnsureDataDirectoryExists(); err != nil {
		return err
	}

	f, err := os.Open(upload.SpoolPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := trnsfr.Server.Filesystem().ExtractStreamUnsafe(ctx, "/", f); err != nil {
		return err
	}

	trnsfr.ReportProgress(transfer.PanelFinalizing, "creating environment", uint64(upload.Length), uint64(upload.Length))
	if err := trnsfr.Server.CreateEnvironment(); err != nil {
		return err
	}

	successful := true
	upload.Completed = true
	upload.CleanupSpool()
	transfer.RemoveTusUpload(upload.ID)
	trnsfr.ReportProgress(transfer.PanelCompleted, "TUS transfer complete", uint64(upload.Length), uint64(upload.Length))
	trnsfr.Log().WithField("transfer_method", "tus").Debug("TUS transfer finalized")
	finalizeIncomingTransfer(manager, trnsfr, &successful)
	return nil
}

func failTusUpload(manager *server.Manager, upload *transfer.TusUpload, err error) {
	if upload == nil {
		return
	}
	if upload.Transfer != nil && err != nil {
		upload.Transfer.SetFailureMessage(err.Error())
	}
	upload.CleanupSpool()
	transfer.RemoveTusUpload(upload.ID)
	if upload.Transfer != nil {
		successful := false
		finalizeIncomingTransfer(manager, upload.Transfer, &successful)
	}
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func parseTusMetadata(header string) map[string]string {
	out := make(map[string]string)
	if header == "" {
		return out
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, " ", 2)
		key := strings.TrimSpace(kv[0])
		if key == "" {
			continue
		}
		if len(kv) == 1 {
			out[key] = ""
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(kv[1]))
		if err != nil {
			continue
		}
		out[key] = string(raw)
	}
	return out
}
