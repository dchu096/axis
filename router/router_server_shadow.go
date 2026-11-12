package router

import (
	stderrors "errors"
	"net/http"
	"os"
	"strings"

	"emperror.dev/errors"
	"github.com/gin-gonic/gin"

	"github.com/0x7d8/wings/router/middleware"
	"github.com/0x7d8/wings/server/backup"
	"github.com/0x7d8/wings/server/shadow"
)

// postServerShadowPreflight checks host free space for a planned shadow extract.
func postServerShadowPreflight(c *gin.Context) {
	var data struct {
		RequiredBytes int64 `json:"required_bytes"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	ok, free, message := shadow.Preflight(data.RequiredBytes)
	if !ok {
		c.JSON(http.StatusOK, gin.H{
			"ok":             false,
			"free_bytes":     free,
			"required_bytes": data.RequiredBytes,
			"message":        message,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":             true,
		"free_bytes":     free,
		"required_bytes": data.RequiredBytes,
	})
}

// postServerShadow starts async materialization of a backup into the shadow dir.
func postServerShadow(c *gin.Context) {
	s := middleware.ExtractServer(c)
	logger := middleware.ExtractLogger(c)

	var data struct {
		ShadowUUID    string             `binding:"required" json:"shadow_uuid"`
		BackupUUID    string             `binding:"required" json:"backup_uuid"`
		Adapter       backup.AdapterType `binding:"required,oneof=wings s3" json:"adapter"`
		DownloadURL   string             `json:"download_url"`
		RequiredBytes int64              `json:"required_bytes"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	req := shadow.CreateRequest{
		ShadowUUID:    data.ShadowUUID,
		BackupUUID:    data.BackupUUID,
		Adapter:       data.Adapter,
		DownloadURL:   data.DownloadURL,
		RequiredBytes: data.RequiredBytes,
	}

	mgr := s.Shadow()
	if mgr == nil {
		middleware.CaptureAndAbort(c, errors.New("shadow manager unavailable"))
		return
	}

	if data.Adapter == backup.LocalBackupAdapter {
		if err := mgr.Create(s.Context(), req, nil); err != nil {
			abortShadowCreate(c, err)
			return
		}
		logger.WithField("shadow", data.ShadowUUID).Info("accepted local shadow create")
		c.Status(http.StatusAccepted)
		return
	}

	if data.DownloadURL == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "The S3 shadow adapter requires a download_url.",
		})
		return
	}
	if err := validateBackupDownloadUrl(data.DownloadURL); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	httpClient := backupRestoreHttpClient()
	httpReq, err := http.NewRequestWithContext(s.Context(), http.MethodGet, data.DownloadURL, nil)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	res, err := httpClient.Do(httpReq)
	if err != nil {
		var downloadErr backupDownloadError
		if stderrors.As(err, &downloadErr) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": downloadErr.Error()})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "The provided backup link returned an invalid response status: " + res.Status,
		})
		return
	}
	if !isSupportedBackupRestoreContentType(res.Header.Get("Content-Type")) {
		_ = res.Body.Close()
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "The provided backup link is not a supported content type. \"" + res.Header.Get("Content-Type") + "\" is not a supported archive type.",
		})
		return
	}

	if err := mgr.Create(s.Context(), req, res.Body); err != nil {
		_ = res.Body.Close()
		abortShadowCreate(c, err)
		return
	}
	logger.WithField("shadow", data.ShadowUUID).Info("accepted S3 shadow create")
	c.Status(http.StatusAccepted)
}

func abortShadowCreate(c *gin.Context, err error) {
	msg := err.Error()
	status := http.StatusBadRequest
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "already being created") {
		status = http.StatusConflict
	}
	c.AbortWithStatusJSON(status, gin.H{"error": msg})
}

// getServerShadow returns the current shadow lifecycle state.
func getServerShadow(c *gin.Context) {
	s := middleware.ExtractServer(c)
	mgr := s.Shadow()
	if mgr == nil {
		c.JSON(http.StatusOK, gin.H{"state": shadow.StateMissing})
		return
	}
	c.JSON(http.StatusOK, mgr.Status())
}

// postServerShadowCancel aborts create and deletes the shadow directory.
func postServerShadowCancel(c *gin.Context) {
	s := middleware.ExtractServer(c)
	mgr := s.Shadow()
	if mgr == nil {
		c.Status(http.StatusNoContent)
		return
	}
	if err := mgr.Cancel(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// deleteServerShadow discards the shadow directory.
func deleteServerShadow(c *gin.Context) {
	s := middleware.ExtractServer(c)
	mgr := s.Shadow()
	if mgr == nil {
		c.Status(http.StatusNoContent)
		return
	}
	if err := mgr.Delete(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// getServerShadowListDirectory lists files inside the shadow tree.
func getServerShadowListDirectory(c *gin.Context) {
	s := middleware.ExtractServer(c)
	mgr := s.Shadow()
	if mgr == nil {
		abortDirectoryNotFound(c)
		return
	}
	dir := c.Query("directory")
	stats, err := mgr.ListDirectory(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			abortDirectoryNotFound(c)
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// postServerShadowPromote copies selected paths from shadow onto the live volume.
func postServerShadowPromote(c *gin.Context) {
	s := middleware.ExtractServer(c)
	mgr := s.Shadow()
	if mgr == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "No shadow is available to promote."})
		return
	}

	var data struct {
		Paths []string `binding:"required" json:"paths"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	if err := mgr.Promote(data.Paths); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
