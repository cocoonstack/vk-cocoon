package snapshots

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/cocoonstack/cocoon-common/manifest"

	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	planPath  = "/v1/snapshot-plan"
	slicePath = "/v1/snapshot-slice"

	sliceChecksumTrailer = "X-Slice-Crc32c"

	// peerSliceBytes caps one plan slice, the checksum unit.
	peerSliceBytes = 256 << 20

	// restoreBufBytes batches stream reads into one write syscall.
	restoreBufBytes = 4 << 20

	// holes up to this size ride along as zeros instead of costing a ~40ms round trip each.
	coalesceGapBytes = 4 << 20

	// 32 concurrent streams saturate the NIC on the measured node classes.
	defaultPeerConcurrency = 32

	// interleaved writers on one inode measured 1.6 GB/s vs 2.7 GB/s for disjoint sequential regions.
	streamsPerFile = 8

	// io.CopyBuffer scratch on both ends; the 32KB default profiled at 12-17% syscall time.
	copyBufBytes = 1 << 20

	// zero-elision granularity: 4K skipping measured half the throughput of 1 MiB.
	zeroSkipBytes = 1 << 20
)

var (
	// hardware CRC: sha256 burned 78-81% of transfer CPU (no SHA-NI on the fleet); guards transport only.
	castagnoli = crc32.MakeTable(crc32.Castagnoli)

	// Two concurrent restores saturate disk and NIC; mirrors pullGate.
	peerRestoreGate = semaphore.NewWeighted(2)

	// one shared transport for connection reuse; dead peers fail fast into the registry fallback.
	peerClient = &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConnsPerHost:   defaultPeerConcurrency,
	}}

	zeroChunk = make([]byte, zeroSkipBytes)
)

type peerPlan struct {
	SnapshotID string     `json:"snapshotId"`
	Files      []peerFile `json:"files"`
}

type peerFile struct {
	Name   string      `json:"name"`
	Size   int64       `json:"size"` // apparent size; holes are never transferred
	Slices []peerSlice `json:"slices"`
}

type peerSlice struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type extent struct {
	offset int64
	length int64
}

// snapshotResolver is the one Runtime call the serve side needs.
type snapshotResolver interface {
	Snapshot(ctx context.Context, name string) (*vm.Snapshot, error)
}

// PeerServer serves local snapshot files to waking peers; StoreDir is cocoon's localfile store root.
type PeerServer struct {
	Snapshots snapshotResolver
	StoreDir  string
}

func (s *PeerServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+planPath, s.handlePlan)
	mux.HandleFunc("GET "+slicePath, s.handleSlice)
	return mux
}

func (s *PeerServer) handlePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	snap, err := s.Snapshots.Snapshot(ctx, name)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, vm.ErrSnapshotNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf("resolve snapshot %s: %v", name, err), status)
		return
	}

	plan, err := s.buildPlan(snap.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("plan snapshot %s: %v", name, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(plan); err != nil {
		log.WithFunc("PeerServer.handlePlan").Errorf(ctx, err, "encode plan for %s", name)
	}
}

func (s *PeerServer) buildPlan(snapshotID string) (*peerPlan, error) {
	dir, err := s.snapshotDir(snapshotID)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	plan := &peerPlan{SnapshotID: snapshotID}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}

		f, err := os.Open(filepath.Join(dir, e.Name())) //nolint:gosec // dir is the resolved store path, e.Name a readdir result
		if err != nil {
			return nil, err
		}

		size, extents, err := scanExtents(f)
		f.Close() //nolint:errcheck,gosec
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", e.Name(), err)
		}

		fileSlices := splitExtents(coalesceExtents(extents, coalesceGapBytes), peerSliceBytes)
		plan.Files = append(plan.Files, peerFile{Name: e.Name(), Size: size, Slices: fileSlices})
	}

	if len(plan.Files) == 0 {
		return nil, fmt.Errorf("snapshot dir %s has no regular files", snapshotID)
	}
	return plan, nil
}

func (s *PeerServer) handleSlice(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, name := q.Get("id"), q.Get("file")
	offset, offErr := strconv.ParseInt(q.Get("offset"), 10, 64)
	length, lenErr := strconv.ParseInt(q.Get("length"), 10, 64)
	if id == "" || name == "" || offErr != nil || lenErr != nil || offset < 0 || length <= 0 {
		http.Error(w, "id, file, offset, length are required", http.StatusBadRequest)
		return
	}

	dir, err := s.snapshotDir(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !isPlainName(name) {
		http.Error(w, "file must be a plain name", http.StatusBadRequest)
		return
	}

	f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // both components validated above
	if err != nil {
		// The snapshot can vanish mid-transfer (delete, GC); the client falls back.
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close() //nolint:errcheck

	w.Header().Set("Trailer", sliceChecksumTrailer)
	w.Header().Set("Content-Type", "application/octet-stream")
	h := crc32.New(castagnoli)
	buf := make([]byte, copyBufBytes)
	n, err := io.CopyBuffer(io.MultiWriter(w, h), io.NewSectionReader(f, offset, length), buf)
	if err != nil || n != length {
		// Body already started; the truncated stream fails the client's length check.
		log.WithFunc("PeerServer.handleSlice").Errorf(r.Context(), err, "stream %s/%s@%d+%d (sent %d)", id, name, offset, length, n)
		return
	}
	w.Header().Set(sliceChecksumTrailer, strconv.FormatUint(uint64(h.Sum32()), 16))
}

// snapshotDir rejects IDs that could escape StoreDir.
func (s *PeerServer) snapshotDir(id string) (string, error) {
	if !isPlainName(id) {
		return "", fmt.Errorf("invalid snapshot id %q", id)
	}
	return filepath.Join(s.StoreDir, id), nil
}

// coalesceExtents merges data runs whose separating hole is at most maxGap.
func coalesceExtents(extents []extent, maxGap int64) []extent {
	var out []extent
	for _, e := range extents {
		if n := len(out); n > 0 && e.offset-(out[n-1].offset+out[n-1].length) <= maxGap {
			out[n-1].length = e.offset + e.length - out[n-1].offset
			continue
		}
		out = append(out, e)
	}
	return out
}

// splitExtents caps each transfer slice at max bytes.
func splitExtents(extents []extent, max int64) []peerSlice {
	var out []peerSlice
	for _, e := range extents {
		for off := e.offset; off < e.offset+e.length; off += max {
			out = append(out, peerSlice{Offset: off, Length: min(max, e.offset+e.length-off)})
		}
	}
	return out
}

// groupSlices splits an ordered slice list into up to n contiguous runs so each fetcher writes one disjoint region.
func groupSlices(sl []peerSlice, n int) iter.Seq[[]peerSlice] {
	return slices.Chunk(sl, max(1, (len(sl)+n-1)/n))
}

// isPlainName rejects path components that could escape their directory.
func isPlainName(s string) bool {
	return s != "" && s != "." && s != ".." && s == filepath.Base(s)
}

// RestoredSnapshot is a staged, clone-ready snapshot directory.
type RestoredSnapshot struct {
	Dir      string
	Snapshot *vm.Snapshot
}

// PeerRestorer fetches raw snapshot files from a peer's vk into a staging dir, buffered and unsynced (~7s faster than O_DIRECT on clone re-read).
type PeerRestorer struct {
	StagingRoot string
}

// Restore fetches name's snapshot files from the peer at baseURL; cfg is the trust anchor, never peer-supplied metadata.
func (p *PeerRestorer) Restore(ctx context.Context, baseURL, name string, cfg *manifest.SnapshotConfig) (_ *RestoredSnapshot, cleanup func(), err error) {
	if acqErr := peerRestoreGate.Acquire(ctx, 1); acqErr != nil {
		return nil, nil, acqErr
	}
	defer peerRestoreGate.Release(1)

	plan, err := p.fetchPlan(ctx, baseURL, name)
	if err != nil {
		return nil, nil, err
	}
	if plan.SnapshotID != cfg.SnapshotID {
		return nil, nil, fmt.Errorf("peer has snapshot %s, registry says %s", plan.SnapshotID, cfg.SnapshotID)
	}

	dir, err := newStagingDir(p.StagingRoot, name)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir) //nolint:errcheck,gosec
		}
	}()

	if err = p.fetchFiles(ctx, baseURL, plan, dir); err != nil {
		return nil, nil, err
	}
	if err = writeEnvelope(dir, cfg, name); err != nil {
		return nil, nil, err
	}

	cleanup = func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			log.WithFunc("PeerRestorer.cleanup").Errorf(context.WithoutCancel(ctx), rmErr, "remove staging dir %s", dir)
		}
	}
	return &RestoredSnapshot{
		Dir: dir,
		Snapshot: &vm.Snapshot{
			ID:          cfg.SnapshotID,
			Name:        name,
			Image:       cfg.Image,
			ImageDigest: cfg.ImageDigest,
			Hypervisor:  cfg.Hypervisor,
		},
	}, cleanup, nil
}

func (p *PeerRestorer) fetchPlan(ctx context.Context, baseURL, name string) (*peerPlan, error) {
	resp, err := p.doGet(ctx, baseURL+planPath+"?name="+url.QueryEscape(name))
	if err != nil {
		return nil, fmt.Errorf("peer plan: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var plan peerPlan
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, fmt.Errorf("peer plan: decode: %w", err)
	}
	return &plan, nil
}

// doGet returns the response only on status 200; any other outcome is an error.
func (p *PeerRestorer) doGet(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := peerClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close() //nolint:errcheck,gosec
		return nil, fmt.Errorf("%s: %s", resp.Status, string(msg))
	}
	return resp, nil
}

func (p *PeerRestorer) fetchFiles(ctx context.Context, baseURL string, plan *peerPlan, dir string) error {
	type task struct {
		file   *os.File
		slices []peerSlice
	}
	var tasks []task
	files := make([]*os.File, 0, len(plan.Files))
	defer func() {
		for _, f := range files {
			f.Close() //nolint:errcheck,gosec
		}
	}()
	for _, pf := range plan.Files {
		if !isPlainName(pf.Name) {
			return fmt.Errorf("peer plan names non-plain file %q", pf.Name)
		}
		for _, sl := range pf.Slices {
			if sl.Offset < 0 || sl.Length <= 0 || sl.Offset+sl.Length > pf.Size {
				return fmt.Errorf("peer plan slice %s@%d+%d out of bounds (size %d)", pf.Name, sl.Offset, sl.Length, pf.Size)
			}
		}
		f, err := os.OpenFile(filepath.Join(dir, pf.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // name validated plain above; cocoon snapshot data files are 0640
		if err != nil {
			return err
		}
		files = append(files, f)
		// Truncate up front so unwritten regions read back as zeros for writeSkippingZeros.
		if err := f.Truncate(pf.Size); err != nil {
			return fmt.Errorf("truncate %s: %w", pf.Name, err)
		}
		for group := range groupSlices(pf.Slices, streamsPerFile) {
			tasks = append(tasks, task{file: f, slices: group})
		}
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(defaultPeerConcurrency)
	for _, t := range tasks {
		eg.Go(func() error {
			for _, sl := range t.slices {
				if err := p.fetchSlice(ctx, baseURL, plan.SnapshotID, t.file, sl); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return eg.Wait()
}

func (p *PeerRestorer) fetchSlice(ctx context.Context, baseURL, snapshotID string, f *os.File, sl peerSlice) error {
	name := filepath.Base(f.Name())
	u := fmt.Sprintf("%s%s?id=%s&file=%s&offset=%d&length=%d",
		baseURL, slicePath, url.QueryEscape(snapshotID), url.QueryEscape(name), sl.Offset, sl.Length)
	resp, err := p.doGet(ctx, u)
	if err != nil {
		return fmt.Errorf("peer slice %s@%d: %w", name, sl.Offset, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	h := crc32.New(castagnoli)
	sw := newSectionWriter(f, sl.Offset)
	buf := make([]byte, copyBufBytes)
	n, err := io.CopyBuffer(io.MultiWriter(sw, h), resp.Body, buf)
	if err != nil {
		return fmt.Errorf("peer slice %s@%d: read: %w", name, sl.Offset, err)
	}
	if err := sw.Flush(); err != nil {
		return fmt.Errorf("peer slice %s@%d: write: %w", name, sl.Offset, err)
	}
	if n != sl.Length {
		return fmt.Errorf("peer slice %s@%d: got %d bytes, want %d", name, sl.Offset, n, sl.Length)
	}
	want := resp.Trailer.Get(sliceChecksumTrailer)
	if want == "" {
		return fmt.Errorf("peer slice %s@%d: server sent no checksum trailer", name, sl.Offset)
	}
	if got := strconv.FormatUint(uint64(h.Sum32()), 16); got != want {
		return fmt.Errorf("peer slice %s@%d: checksum mismatch", name, sl.Offset)
	}
	return nil
}

// sectionWriter batches one slice's stream into restoreBufBytes writes at its file offset.
type sectionWriter struct {
	f   *os.File
	off int64
	buf []byte
	n   int
}

func newSectionWriter(f *os.File, off int64) *sectionWriter {
	return &sectionWriter{f: f, off: off, buf: make([]byte, restoreBufBytes)}
}

func (s *sectionWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		c := copy(s.buf[s.n:], p)
		s.n += c
		p = p[c:]
		if s.n == len(s.buf) {
			if err := s.Flush(); err != nil {
				return total - len(p), err
			}
		}
	}
	return total, nil
}

// Flush drains what Write batched; call once after the stream ends.
func (s *sectionWriter) Flush() error {
	if s.n == 0 {
		return nil
	}
	if err := writeSkippingZeros(s.f, s.buf[:s.n], s.off); err != nil {
		return err
	}
	s.off += int64(s.n)
	s.n = 0
	return nil
}

// writeSkippingZeros writes data at off, eliding zeroSkipBytes-granular all-zero chunks so skipped regions stay sparse.
func writeSkippingZeros(f *os.File, data []byte, off int64) error {
	for start := 0; start < len(data); {
		end := min(start+zeroSkipBytes, len(data))
		if bytes.Equal(data[start:end], zeroChunk[:end-start]) {
			start = end
			continue
		}
		runStart := start
		for start < len(data) {
			end = min(start+zeroSkipBytes, len(data))
			if bytes.Equal(data[start:end], zeroChunk[:end-start]) {
				break
			}
			start = end
		}
		if _, err := f.WriteAt(data[runStart:start], off+int64(runStart)); err != nil {
			return err
		}
	}
	return nil
}
