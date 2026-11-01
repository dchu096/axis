package backup

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/mholt/archives"
	"golang.org/x/sync/errgroup"

	"github.com/0x7d8/wings/config"
	"github.com/0x7d8/wings/remote"
	"github.com/0x7d8/wings/server/filesystem"
)

type AdapterType string

const (
	LocalBackupAdapter AdapterType = "wings"
	S3BackupAdapter    AdapterType = "s3"
)

// RestoreCallback is a generic restoration callback that exists for both local
// and remote backups allowing the files to be restored.
type RestoreCallback func(file string, info fs.FileInfo, r io.ReadCloser) error

// noinspection GoNameStartsWithPackageName
type BackupInterface interface {
	// SetClient sets the API request client on the backup interface.
	SetClient(remote.Client)
	// Identifier returns the UUID of this backup as tracked by the panel
	// instance.
	Identifier() string
	// WithLogContext attaches additional context to the log output for this
	// backup.
	WithLogContext(map[string]interface{})
	// Generate creates a backup in whatever the configured source for the
	// specific implementation is.
	Generate(context.Context, *filesystem.Filesystem, string) (*ArchiveDetails, error)
	// Ignored returns the ignored files for this backup instance.
	Ignored() string
	// Checksum returns a SHA1 checksum for the generated backup.
	Checksum() ([]byte, error)
	// Size returns the size of the generated backup.
	Size() (int64, error)
	// Path returns the path to the backup on the machine. This is not always
	// the final storage location of the backup, simply the location we're using
	// to store it until it is moved to the final spot.
	Path() string
	// Details returns details about the archive.
	Details(context.Context, []remote.BackupPart) (*ArchiveDetails, error)
	// Remove removes a backup file.
	Remove() error
	// Restore is called when a backup is ready to be restored to the disk from
	// the given source. Not every backup implementation will support this nor
	// will every implementation require a reader be provided.
	Restore(context.Context, io.Reader, RestoreCallback) error
}

type Backup struct {
	// The UUID of this backup object. This must line up with a backup from
	// the panel instance.
	Uuid string `json:"uuid"`

	// An array of files to ignore when generating this backup. This should be
	// compatible with a standard .gitignore structure.
	Ignore string `json:"ignore"`

	client     remote.Client
	adapter    AdapterType
	logContext map[string]interface{}

	// foundPath is set when locating an existing archive that may use a
	// different extension than the currently configured format.
	foundPath string
}

func (b *Backup) SetClient(c remote.Client) {
	b.client = c
}

func (b *Backup) Identifier() string {
	return b.Uuid
}

func (b *Backup) normalizedIdentifier() (string, error) {
	parsed, err := uuid.Parse(b.Identifier())
	if err != nil || len(b.Identifier()) != len(parsed.String()) || parsed.String() != strings.ToLower(b.Identifier()) {
		return "", errors.New("backup: identifier must be a valid UUID")
	}
	return parsed.String(), nil
}

func (b *Backup) validateIdentifier() error {
	identifier, err := b.normalizedIdentifier()
	if err != nil {
		return err
	}
	b.Uuid = identifier
	return nil
}

// ArchiveExtension is the on-disk / upload extension for newly created backups.
func ArchiveExtension() string {
	return ".tar.zst"
}

// ArchiveContentType is the Content-Type used when uploading backup parts to S3/R2.
func ArchiveContentType() string {
	return "application/zstd"
}

// Path returns the path for this specific backup (.tar.zst), or a previously
// located on-disk path when set (may be a legacy .tar.gz).
func (b *Backup) Path() string {
	if b.foundPath != "" {
		return b.foundPath
	}
	identifier, err := b.normalizedIdentifier()
	if err != nil {
		identifier = path.Base(b.Identifier())
	}
	return path.Join(config.Get().System.BackupDirectory, identifier+ArchiveExtension())
}

// candidatePaths returns on-disk paths to check when locating an existing backup
// (.tar.zst first, then legacy .tar.gz).
func (b *Backup) candidatePaths() []string {
	identifier, err := b.normalizedIdentifier()
	if err != nil {
		identifier = path.Base(b.Identifier())
	}
	dir := config.Get().System.BackupDirectory
	return []string{
		path.Join(dir, identifier+".tar.zst"),
		path.Join(dir, identifier+".tar.gz"),
	}
}

// Size returns the size of the generated backup.
func (b *Backup) Size() (int64, error) {
	if err := b.validateIdentifier(); err != nil {
		return 0, err
	}
	st, err := os.Stat(b.Path())
	if err != nil {
		return 0, err
	}

	return st.Size(), nil
}

// Checksum returns the SHA256 checksum of a backup.
func (b *Backup) Checksum() ([]byte, error) {
	if err := b.validateIdentifier(); err != nil {
		return nil, err
	}
	h := sha1.New()

	f, err := os.Open(b.Path())
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, 1024*4)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}

// Details returns both the checksum and size of the archive currently stored on
// the disk to the caller.
func (b *Backup) Details(ctx context.Context, parts []remote.BackupPart) (*ArchiveDetails, error) {
	ad := ArchiveDetails{ChecksumType: "sha1", Parts: parts}
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		resp, err := b.Checksum()
		if err != nil {
			return err
		}
		ad.Checksum = hex.EncodeToString(resp)
		return nil
	})

	g.Go(func() error {
		s, err := b.Size()
		if err != nil {
			return err
		}
		ad.Size = s
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, errors.WithStackDepth(err, 1)
	}
	return &ad, nil
}

func (b *Backup) Ignored() string {
	return b.Ignore
}

// Returns a logger instance for this backup with the additional context fields
// assigned to the output.
func (b *Backup) log() *log.Entry {
	l := log.WithField("backup", b.Identifier()).WithField("adapter", b.adapter)
	for k, v := range b.logContext {
		l = l.WithField(k, v)
	}
	return l
}

// extractArchive restores files from a gzip or zstd compressed tar stream,
// detecting the codec from magic bytes.
func extractArchive(ctx context.Context, r io.Reader, callback RestoreCallback) error {
	format, input, err := archives.Identify(ctx, "backup.tar", r)
	if err != nil {
		return errors.Wrap(err, "backup: identify archive format")
	}
	extractor, ok := format.(archives.Extractor)
	if !ok {
		return errors.New("backup: identified format is not an archive extractor")
	}
	return extractor.Extract(ctx, input, func(ctx context.Context, f archives.FileInfo) error {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return callback(f.NameInArchive, f.FileInfo, rc)
	})
}

type ArchiveDetails struct {
	Checksum     string              `json:"checksum"`
	ChecksumType string              `json:"checksum_type"`
	Size         int64               `json:"size"`
	Parts        []remote.BackupPart `json:"parts"`
}

// ToRequest returns a request object.
func (ad *ArchiveDetails) ToRequest(successful bool) remote.BackupRequest {
	return remote.BackupRequest{
		Checksum:     ad.Checksum,
		ChecksumType: ad.ChecksumType,
		Size:         ad.Size,
		Successful:   successful,
		Parts:        ad.Parts,
	}
}
