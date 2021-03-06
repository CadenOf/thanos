// Package shipper detects directories on the local file system and uploads
// them to a block storage.
package shipper

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"math"
	"os"
	"path"
	"path/filepath"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/improbable-eng/thanos/pkg/block"
	"github.com/improbable-eng/thanos/pkg/objstore"
	"github.com/improbable-eng/thanos/pkg/runutil"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/tsdb/fileutil"
	"github.com/prometheus/tsdb/labels"
)

type metrics struct {
	dirSyncs        prometheus.Counter
	dirSyncFailures prometheus.Counter
	uploads         prometheus.Counter
	uploadFailures  prometheus.Counter
}

func newMetrics(r prometheus.Registerer) *metrics {
	var m metrics

	m.dirSyncs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "thanos_shipper_dir_syncs_total",
		Help: "Total dir sync attempts",
	})
	m.dirSyncFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "thanos_shipper_dir_sync_failures_total",
		Help: "Total number of failed dir syncs",
	})
	m.uploads = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "thanos_shipper_uploads_total",
		Help: "Total object upload attempts",
	})
	m.uploadFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "thanos_shipper_upload_failures_total",
		Help: "Total number of failed object uploads",
	})

	if r != nil {
		r.MustRegister(
			m.dirSyncs,
			m.dirSyncFailures,
			m.uploads,
			m.uploadFailures,
		)
	}
	return &m
}

// Shipper watches a directory for matching files and directories and uploads
// them to a remote data store.
type Shipper struct {
	logger  log.Logger
	dir     string
	metrics *metrics
	bucket  objstore.Bucket
	labels  func() labels.Labels
	source  block.SourceType
}

// New creates a new shipper that detects new TSDB blocks in dir and uploads them
// to remote if necessary. It attaches the return value of the labels getter to uploaded data.
func New(
	logger log.Logger,
	r prometheus.Registerer,
	dir string,
	bucket objstore.Bucket,
	lbls func() labels.Labels,
	source block.SourceType,
) *Shipper {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	if lbls == nil {
		lbls = func() labels.Labels { return nil }
	}
	return &Shipper{
		logger:  logger,
		dir:     dir,
		bucket:  bucket,
		labels:  lbls,
		metrics: newMetrics(r),
		source:  source,
	}
}

// Timestamps returns the minimum timestamp for which data is available and the highest timestamp
// of blocks that were successfully uploaded.
func (s *Shipper) Timestamps() (minTime, maxSyncTime int64, err error) {
	meta, err := ReadMetaFile(s.dir)
	if err != nil {
		return 0, 0, errors.Wrap(err, "read shipper meta file")
	}
	// Build a map of blocks we already uploaded.
	hasUploaded := make(map[ulid.ULID]struct{}, len(meta.Uploaded))
	for _, id := range meta.Uploaded {
		hasUploaded[id] = struct{}{}
	}

	minTime = math.MaxInt64
	maxSyncTime = math.MinInt64

	if err := s.iterBlockMetas(func(m *block.Meta) error {
		if m.MinTime < minTime {
			minTime = m.MinTime
		}
		if _, ok := hasUploaded[m.ULID]; ok && m.MaxTime > maxSyncTime {
			maxSyncTime = m.MaxTime
		}
		return nil
	}); err != nil {
		return 0, 0, errors.Wrap(err, "iter Block metas for timestamp")
	}

	if minTime == math.MaxInt64 {
		// No block yet found. We cannot assume any min block size so propagate 0 minTime.
		minTime = 0
	}

	return minTime, maxSyncTime, nil
}

// Sync performs a single synchronization, which ensures all local blocks have been uploaded
// to the object bucket once.
// It is not concurrency-safe.
func (s *Shipper) Sync(ctx context.Context) {
	meta, err := ReadMetaFile(s.dir)
	if err != nil {
		// If we encounter any error, proceed with an empty meta file and overwrite it later.
		// The meta file is only used to deduplicate uploads, which are properly handled
		// by the system if their occur anyway.
		if !os.IsNotExist(err) {
			level.Warn(s.logger).Log("msg", "reading meta file failed, removing it", "err", err)
		}
		meta = &Meta{Version: 1}
	}
	// Build a map of blocks we already uploaded.
	hasUploaded := make(map[ulid.ULID]struct{}, len(meta.Uploaded))
	for _, id := range meta.Uploaded {
		hasUploaded[id] = struct{}{}
	}
	// Reset the uploaded slice so we can rebuild it only with blocks that still exist locally.
	meta.Uploaded = nil

	// TODO(bplotka): If there are no blocks in the system check for WAL dir to ensure we have actually
	// access to real TSDB dir (!).
	if err = s.iterBlockMetas(func(m *block.Meta) error {
		// Do not sync a block if we already uploaded it. If it is no longer found in the bucket,
		// it was generally removed by the compaction process.
		if _, ok := hasUploaded[m.ULID]; !ok {
			if err := s.sync(ctx, m); err != nil {
				level.Error(s.logger).Log("msg", "shipping failed", "block", m.ULID, "err", err)
				// No error returned, just log line. This is because we want other blocks to be uploaded even
				// though this one failed. It will be retried on second Sync iteration.
				return nil
			}
		}
		meta.Uploaded = append(meta.Uploaded, m.ULID)
		return nil
	}); err != nil {
		level.Error(s.logger).Log("msg", "iter block metas failed", "err", err)
		return
	}
	if err := WriteMetaFile(s.logger, s.dir, meta); err != nil {
		level.Warn(s.logger).Log("msg", "updating meta file failed", "err", err)
	}
}

func (s *Shipper) sync(ctx context.Context, meta *block.Meta) (err error) {
	dir := filepath.Join(s.dir, meta.ULID.String())

	// We only ship of the first compacted block level.
	// TODO(bplotka): https://github.com/improbable-eng/thanos/issues/206
	if meta.Compaction.Level > 1 {
		return nil
	}

	// Check against bucket if the meta file for this block exists.
	ok, err := s.bucket.Exists(ctx, path.Join(meta.ULID.String(), block.MetaFilename))
	if err != nil {
		return errors.Wrap(err, "check exists")
	}
	if ok {
		return nil
	}

	level.Info(s.logger).Log("msg", "upload new block", "id", meta.ULID)

	// We hard-link the files into a temporary upload directory so we are not affected
	// by other operations happening against the TSDB directory.
	updir := filepath.Join(s.dir, "thanos", "upload", meta.ULID.String())

	// Remove updir just in case.
	if err := os.RemoveAll(updir); err != nil {
		return errors.Wrap(err, "clean upload directory")
	}
	if err := os.MkdirAll(updir, 0777); err != nil {
		return errors.Wrap(err, "create upload dir")
	}
	defer func() {
		if err := os.RemoveAll(updir); err != nil {
			level.Error(s.logger).Log("msg", "failed to clean upload directory", "err", err)
		}
	}()

	if err := hardlinkBlock(dir, updir); err != nil {
		return errors.Wrap(err, "hard link block")
	}
	// Attach current labels and write a new meta file with Thanos extensions.
	if lset := s.labels(); lset != nil {
		meta.Thanos.Labels = lset.Map()
	}
	meta.Thanos.Source = s.source
	if err := block.WriteMetaFile(s.logger, updir, meta); err != nil {
		return errors.Wrap(err, "write meta file")
	}
	return block.Upload(ctx, s.logger, s.bucket, updir)
}

// iterBlockMetas calls f with the block meta for each block found in dir. It logs
// an error and continues if it cannot access a meta.json file.
// If f returns an error, the function returns with the same error.
func (s *Shipper) iterBlockMetas(f func(m *block.Meta) error) error {
	names, err := fileutil.ReadDir(s.dir)
	if err != nil {
		return errors.Wrap(err, "read dir")
	}
	for _, n := range names {
		if _, ok := block.IsBlockDir(n); !ok {
			continue
		}
		dir := filepath.Join(s.dir, n)

		fi, err := os.Stat(dir)
		if err != nil {
			level.Warn(s.logger).Log("msg", "open file failed", "err", err)
			continue
		}
		if !fi.IsDir() {
			continue
		}
		m, err := block.ReadMetaFile(dir)
		if err != nil {
			level.Warn(s.logger).Log("msg", "reading meta file failed", "err", err)
			continue
		}
		if err := f(m); err != nil {
			return err
		}
	}
	return nil
}

func hardlinkBlock(src, dst string) error {
	chunkDir := filepath.Join(dst, block.ChunksDirname)

	if err := os.MkdirAll(chunkDir, 0777); err != nil {
		return errors.Wrap(err, "create chunks dir")
	}

	files, err := fileutil.ReadDir(filepath.Join(src, block.ChunksDirname))
	if err != nil {
		return errors.Wrap(err, "read chunk dir")
	}
	for i, fn := range files {
		files[i] = filepath.Join(block.ChunksDirname, fn)
	}
	files = append(files, block.MetaFilename, block.IndexFilename)

	for _, fn := range files {
		if err := os.Link(filepath.Join(src, fn), filepath.Join(dst, fn)); err != nil {
			return errors.Wrapf(err, "hard link file %s", fn)
		}
	}
	return nil
}

// Meta defines the fomart thanos.shipper.json file that the shipper places in the data directory.
type Meta struct {
	Version  int         `json:"version"`
	Uploaded []ulid.ULID `json:"uploaded"`
}

// MetaFilename is the known JSON filename for meta information.
const MetaFilename = "thanos.shipper.json"

// WriteMetaFile writes the given meta into <dir>/thanos.shipper.json.
func WriteMetaFile(logger log.Logger, dir string, meta *Meta) error {
	// Make any changes to the file appear atomic.
	path := filepath.Join(dir, MetaFilename)
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(f)
	enc.SetIndent("", "\t")

	if err := enc.Encode(meta); err != nil {
		runutil.CloseWithLogOnErr(logger, f, "write meta file close")
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameFile(logger, tmp, path)
}

// ReadMetaFile reads the given meta from <dir>/thanos.shipper.json.
func ReadMetaFile(dir string) (*Meta, error) {
	b, err := ioutil.ReadFile(filepath.Join(dir, MetaFilename))
	if err != nil {
		return nil, err
	}
	var m Meta

	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != 1 {
		return nil, errors.Errorf("unexpected meta file version %d", m.Version)
	}

	return &m, nil
}

func renameFile(logger log.Logger, from, to string) error {
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}

	// Directory was renamed; sync parent dir to persist rename.
	pdir, err := fileutil.OpenDir(filepath.Dir(to))
	if err != nil {
		return err
	}

	if err = fileutil.Fsync(pdir); err != nil {
		runutil.CloseWithLogOnErr(logger, pdir, "rename file dir close")
		return err
	}
	return pdir.Close()
}
