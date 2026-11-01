package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// PushTusToTarget builds a local archive spool and uploads it to the destination
// via TUS 1.0 (POST create + PATCH chunks with HEAD resume).
func (t *Transfer) PushTusToTarget(ctx context.Context, createURL, token string, chunkSize int64) error {
	t.Method = "tus"
	t.SetStatus(StatusProcessing)
	t.tusBytesCopied.Store(0)
	t.ReportProgress(PanelPreparing, "building archive spool", 0, 0)
	t.SendMessage("Building archive for TUS transfer...")

	spoolPath, size, checksum, err := t.buildArchiveSpool(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(spoolPath) }()

	chunkSize = EffectiveChunkSize(chunkSize)
	t.ReportProgress(PanelCopying, "creating TUS upload", 0, uint64(size))
	t.SendMessage("Creating TUS upload on destination...")

	location, err := t.tusCreate(ctx, createURL, token, size, checksum)
	if err != nil {
		return err
	}
	t.tusUploadURL = location
	t.tusToken = token

	// Best-effort terminate remote upload if we are cancelled mid-flight.
	defer func() {
		if ctx.Err() != nil && location != "" {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = tusDelete(cancelCtx, location, token)
		}
	}()

	t.SendMessage("Uploading archive via TUS...")
	stopHB := t.StartCopyHeartbeat(20*time.Second, "TUS upload in progress", func() (uint64, uint64) {
		return t.tusBytesCopied.Load(), uint64(size)
	})
	defer stopHB()

	if err := t.tusPatchAll(ctx, location, token, spoolPath, size, chunkSize); err != nil {
		return err
	}

	t.ReportProgress(PanelVerifying, "TUS upload complete; waiting for destination", uint64(size), uint64(size))
	t.SendMessage("TUS upload complete; destination is finalizing...")
	t.Log().WithField("transfer_method", "tus").Debug("finished TUS push to destination")
	return nil
}

func (t *Transfer) buildArchiveSpool(ctx context.Context) (path string, size int64, checksumHex string, err error) {
	if err := ensureSpoolDirectory(); err != nil {
		return "", 0, "", err
	}

	a, err := t.Archive()
	if err != nil {
		t.Error(err, "Failed to get archive for transfer.")
		return "", 0, "", fmt.Errorf("failed to get archive for transfer: %w", err)
	}

	f, err := os.CreateTemp(SpoolDirectory(), "out-*.tar.zst")
	if err != nil {
		return "", 0, "", fmt.Errorf("tus: create source spool: %w", err)
	}
	path = f.Name()

	h := sha256.New()
	w := io.MultiWriter(f, h)

	if err := a.Stream(ctx, w); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		if ctx.Err() != nil {
			return "", 0, "", ctx.Err()
		}
		return "", 0, "", fmt.Errorf("tus: stream archive to spool: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", 0, "", err
	}

	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return "", 0, "", err
	}
	return path, info.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

func (t *Transfer) tusCreate(ctx context.Context, createURL, token string, length int64, checksumHex string) (string, error) {
	meta := encodeTusMetadata(map[string]string{
		"filename":    "archive.tar.zst",
		"server_uuid": t.Server.ID(),
		"sha256":      checksumHex,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Tus-Resumable", TusResumableVersion)
	req.Header.Set("Upload-Length", strconv.FormatInt(length, 10))
	req.Header.Set("Upload-Metadata", meta)

	client := &http.Client{Timeout: 0}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))

	if res.StatusCode != http.StatusCreated {
		if isTusUnsupportedStatus(res.StatusCode) {
			return "", &TusUnsupportedError{
				Status: res.StatusCode,
				Body:   string(body),
			}
		}
		return "", fmt.Errorf("tus create: unexpected status %d: %s", res.StatusCode, string(body))
	}

	location := res.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("tus create: missing Location header")
	}
	return resolveTusLocation(createURL, location)
}

func (t *Transfer) tusPatchAll(ctx context.Context, location, token, spoolPath string, length, chunkSize int64) error {
	f, err := os.Open(spoolPath)
	if err != nil {
		return err
	}
	defer f.Close()

	offset := int64(0)
	if off, err := tusHeadOffset(ctx, location, token); err == nil && off > 0 {
		offset = off
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		t.tusBytesCopied.Store(uint64(offset))
	}

	client := &http.Client{Timeout: 0}
	buf := make([]byte, chunkSize)
	retries := 0
	const maxRetries = 5

	for offset < length {
		if err := ctx.Err(); err != nil {
			return err
		}

		toRead := chunkSize
		remaining := length - offset
		if remaining < toRead {
			toRead = remaining
		}
		n, readErr := io.ReadFull(f, buf[:toRead])
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if n == 0 {
			break
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, location, bytes.NewReader(buf[:n]))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", token)
		req.Header.Set("Tus-Resumable", TusResumableVersion)
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		req.ContentLength = int64(n)

		res, err := client.Do(req)
		if err != nil {
			retries++
			if retries > maxRetries {
				return fmt.Errorf("tus patch: %w", err)
			}
			t.Log().WithError(err).Warn("TUS PATCH network error; probing offset to resume")
			if off, headErr := tusHeadOffset(ctx, location, token); headErr == nil {
				offset = off
				if _, seekErr := f.Seek(offset, io.SeekStart); seekErr != nil {
					return seekErr
				}
				t.tusBytesCopied.Store(uint64(offset))
			}
			time.Sleep(time.Duration(retries) * time.Second)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		_ = res.Body.Close()

		if res.StatusCode == http.StatusConflict {
			retries++
			if retries > maxRetries {
				return fmt.Errorf("tus patch: offset conflict after retries: %s", string(body))
			}
			off, headErr := tusHeadOffset(ctx, location, token)
			if headErr != nil {
				return fmt.Errorf("tus patch conflict and HEAD failed: %w", headErr)
			}
			offset = off
			if _, seekErr := f.Seek(offset, io.SeekStart); seekErr != nil {
				return seekErr
			}
			t.tusBytesCopied.Store(uint64(offset))
			continue
		}

		if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK {
			return fmt.Errorf("tus patch: unexpected status %d: %s", res.StatusCode, string(body))
		}

		newOffset, err := strconv.ParseInt(res.Header.Get("Upload-Offset"), 10, 64)
		if err != nil || newOffset <= offset {
			newOffset = offset + int64(n)
		}
		offset = newOffset
		t.tusBytesCopied.Store(uint64(offset))
		retries = 0

		t.ReportProgress(PanelCopying, "TUS upload in progress", uint64(offset), uint64(length))
	}

	if offset < length {
		return fmt.Errorf("tus patch: incomplete upload offset=%d length=%d", offset, length)
	}
	return nil
}

func tusHeadOffset(ctx context.Context, location, token string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, location, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Tus-Resumable", TusResumableVersion)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("tus head: status %d", res.StatusCode)
	}
	return strconv.ParseInt(res.Header.Get("Upload-Offset"), 10, 64)
}

func tusDelete(ctx context.Context, location, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, location, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Tus-Resumable", TusResumableVersion)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return nil
}

// TusUnsupportedError indicates the destination does not speak TUS (safe archive fallback).
type TusUnsupportedError struct {
	Status int
	Body   string
}

func (e *TusUnsupportedError) Error() string {
	return fmt.Sprintf("tus unsupported: status %d: %s", e.Status, e.Body)
}

// IsTusUnsupported reports whether err is a safe archive-fallback condition.
func IsTusUnsupported(err error) bool {
	_, ok := err.(*TusUnsupportedError)
	return ok
}

func isTusUnsupportedStatus(code int) bool {
	return code == http.StatusNotFound ||
		code == http.StatusMethodNotAllowed ||
		code == http.StatusNotImplemented
}

func encodeTusMetadata(pairs map[string]string) string {
	parts := make([]string, 0, len(pairs))
	for k, v := range pairs {
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}

func resolveTusLocation(createURL, location string) (string, error) {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return location, nil
	}
	base, err := url.Parse(createURL)
	if err != nil {
		return "", fmt.Errorf("tus create: parse create url: %w", err)
	}
	ref, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("tus create: parse location: %w", err)
	}
	return base.ResolveReference(ref).String(), nil
}