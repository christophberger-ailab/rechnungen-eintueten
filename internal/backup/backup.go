// Package backup replicates the SQLite database to S3-compatible object
// storage with Litestream, embedded in this process.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/s3"
)

// Config describes where the database is replicated to.
type Config struct {
	Endpoint        string // e.g. "https://fsn1.your-objectstorage.com"
	Region          string // e.g. "fsn1"
	Bucket          string
	Path            string // key prefix inside the bucket
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	SyncInterval    time.Duration // zero means the Litestream default
}

// Validate reports whether the configuration is complete enough to use.
func (c Config) Validate() error {
	// These three are the minimum Litestream needs to address and
	// authenticate against a bucket; everything else has a usable default.
	if c.Bucket == "" {
		return fmt.Errorf("backup: bucket is not configured")
	}
	if c.AccessKeyID == "" {
		return fmt.Errorf("backup: access key id is not configured")
	}
	if c.SecretAccessKey == "" {
		return fmt.Errorf("backup: secret access key is not configured")
	}
	return nil
}

// replicaClient builds the Litestream S3 replica client for c. Litestream's
// S3 client also speaks the S3-compatible APIs of providers such as Hetzner
// Object Storage once Endpoint and ForcePathStyle are set accordingly.
func (c Config) replicaClient(logger *slog.Logger) *s3.ReplicaClient {
	client := s3.NewReplicaClient()
	client.Endpoint = c.Endpoint
	client.Region = c.Region
	client.Bucket = c.Bucket
	client.Path = c.Path
	client.AccessKeyID = c.AccessKeyID
	client.SecretAccessKey = c.SecretAccessKey
	client.ForcePathStyle = c.ForcePathStyle
	client.SetLogger(logger)
	return client
}

// Replicator streams a SQLite database to object storage.
type Replicator struct {
	db    *litestream.DB
	store *litestream.Store
}

// Start begins replicating the database at dbPath. The returned
// Replicator must be stopped to flush the final changes.
func Start(ctx context.Context, dbPath string, cfg Config, logger *slog.Logger) (*Replicator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	logger = orDefault(logger)

	return start(ctx, dbPath, cfg.replicaClient(logger), cfg.SyncInterval, logger)
}

// start drives replication for any Litestream client, so tests can exercise
// the real Store/Replica/DB wiring with the file backend instead of S3.
func start(ctx context.Context, dbPath string, client litestream.ReplicaClient, syncInterval time.Duration, logger *slog.Logger) (*Replicator, error) {
	logger = orDefault(logger)
	db := litestream.NewDB(dbPath)
	replica := litestream.NewReplicaWithClient(db, client)
	if syncInterval > 0 {
		replica.SyncInterval = syncInterval
	}
	db.Replica = replica

	// A Store is what actually opens the DB and drives background
	// replication/compaction; litestream.DefaultCompactionLevels is the
	// same level set the litestream CLI uses. NewStore stamps its own
	// default logger onto db as a side effect, so re-apply ours after.
	store := litestream.NewStore([]*litestream.DB{db}, litestream.DefaultCompactionLevels)
	store.Logger = logger
	db.SetLogger(logger)

	if err := store.Open(ctx); err != nil {
		return nil, fmt.Errorf("backup: start replication for %s: %w", dbPath, err)
	}
	return &Replicator{db: db, store: store}, nil
}

// Stop flushes pending changes and stops replication.
func (r *Replicator) Stop(ctx context.Context) error {
	if err := r.store.Close(ctx); err != nil {
		return fmt.Errorf("backup: stop replication: %w", err)
	}
	return nil
}

// Restore writes the newest replicated copy of the database to dbPath,
// which must not exist yet. It is the disaster-recovery path: the
// configuration cannot come from the database being restored.
func Restore(ctx context.Context, dbPath string, cfg Config, logger *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger = orDefault(logger)

	// Litestream's own restore would happily overwrite dbPath; check first
	// so a mistaken restore can never clobber a live database file.
	if _, err := os.Stat(dbPath); err == nil {
		return fmt.Errorf("backup: restore target already exists: %s", dbPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: stat restore target: %w", err)
	}

	return restore(ctx, dbPath, cfg.replicaClient(logger))
}

// restore drives a restore for any Litestream client, so tests can exercise
// the real Replica.Restore path with the file backend instead of S3.
func restore(ctx context.Context, dbPath string, client litestream.ReplicaClient) error {
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("backup: init object storage client: %w", err)
	}

	// No *litestream.DB is attached; the client alone is enough to list
	// and fetch LTX files for a cold-start restore onto an empty path.
	replica := litestream.NewReplicaWithClient(nil, client)
	opt := litestream.NewRestoreOptions()
	opt.OutputPath = dbPath
	if err := replica.Restore(ctx, opt); err != nil {
		return fmt.Errorf("backup: restore %s: %w", dbPath, err)
	}
	return nil
}

// orDefault returns logger, or the process-wide default if logger is nil.
func orDefault(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}
