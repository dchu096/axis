package backup

import (
	"context"
	"io"
	"os"

	"emperror.dev/errors"
	"github.com/juju/ratelimit"

	"github.com/0x7d8/wings/config"
	"github.com/0x7d8/wings/remote"
	"github.com/0x7d8/wings/server/filesystem"
)

type LocalBackup struct {
	Backup
}

var _ BackupInterface = (*LocalBackup)(nil)

func NewLocal(client remote.Client, uuid string, ignore string) *LocalBackup {
	return &LocalBackup{
		Backup: Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: LocalBackupAdapter,
		},
	}
}

// LocateLocal finds the backup for a server and returns the local path. This
// will obviously only work if the backup was created as a local backup.
func LocateLocal(client remote.Client, uuid string) (*LocalBackup, os.FileInfo, error) {
	b := NewLocal(client, uuid, "")
	if err := b.validateIdentifier(); err != nil {
		return nil, nil, err
	}

	var lastErr error
	for _, p := range b.candidatePaths() {
		st, err := os.Stat(p)
		if err != nil {
			lastErr = err
			continue
		}
		if st.IsDir() {
			return nil, nil, errors.New("invalid archive, is directory")
		}
		b.foundPath = p
		return b, st, nil
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return nil, nil, lastErr
}

// Remove removes a backup from the system.
func (b *LocalBackup) Remove() error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}
	err := os.Remove(b.Path())
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, p := range b.candidatePaths() {
		if p == b.Path() {
			continue
		}
		if rerr := os.Remove(p); rerr == nil || errors.Is(rerr, os.ErrNotExist) {
			return nil
		} else {
			return rerr
		}
	}
	return err
}

// WithLogContext attaches additional context to the log output for this backup.
func (b *LocalBackup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// Generate generates a backup of the selected files and pushes it to the
// defined location for this instance.
func (b *LocalBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := b.validateIdentifier(); err != nil {
		return nil, err
	}
	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
	}

	b.log().WithField("path", b.Path()).Info("creating backup for server")
	if err := a.Create(ctx, b.Path()); err != nil {
		return nil, err
	}
	b.log().Info("created backup successfully")

	ad, err := b.Details(ctx, nil)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details for local backup")
	}
	return ad, nil
}

// Restore will walk over the archive and call the callback function for each
// file encountered.
func (b *LocalBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}
	f, err := os.Open(b.Path())
	if err != nil {
		return err
	}
	defer f.Close()

	var reader io.Reader = f
	// Steal the logic we use for making backups which will be applied when restoring
	// this specific backup. This allows us to prevent overloading the disk unintentionally.
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		reader = ratelimit.Reader(f, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	}
	return extractArchive(ctx, reader, callback)
}
