package archiver

import (
	"context"
	"sort"
	"sync"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/fs"
)

// Scanner  traverses the targets and calls the function Result with cumulated
// stats concerning the files and folders found. Select is used to decide which
// items should be included. Error is called when an error occurs.
type Scanner struct {
	FS           fs.FS
	SelectByName SelectByNameFunc
	Select       SelectFunc
	Error        ErrorFunc
	Result       func(item string, s ScanStats)

	// Concurrency sets how many directory entries are scanned at the same
	// time, like Options.WalkConcurrency does for the archiver. Zero and one
	// scan sequentially.
	Concurrency uint

	mu      sync.Mutex
	total   ScanStats
	walkers chan struct{}
}

// NewScanner initializes a new Scanner.
func NewScanner(filesystem fs.FS) *Scanner {
	return &Scanner{
		FS:           filesystem,
		SelectByName: func(_ string) bool { return true },
		Select:       func(_ string, _ *fs.ExtendedFileInfo, _ fs.FS) bool { return true },
		Error:        func(_ string, err error) error { return err },
		Result:       func(_ string, _ ScanStats) {},
	}
}

// ScanStats collect statistics.
type ScanStats struct {
	Files, Dirs, Others uint
	Bytes               uint64
}

func (s *Scanner) scanTree(ctx context.Context, tree tree) error {
	// traverse the path in the file system for all leaf nodes
	if tree.Leaf() {
		abstarget, err := s.FS.Abs(tree.Path)
		if err != nil {
			return err
		}

		return s.scan(ctx, abstarget, tree.Explicit)
	}

	// otherwise recurse into the nodes in a deterministic order
	for _, name := range tree.NodeNames() {
		if err := s.scanTree(ctx, tree.Nodes[name]); err != nil {
			return err
		}

		if ctx.Err() != nil {
			return nil
		}
	}

	return nil
}

// Scan traverses the targets. The function Result is called for each new item
// found, the complete result is also returned by Scan.
func (s *Scanner) Scan(ctx context.Context, targets []string) error {
	debug.Log("start scan for %v", targets)

	cleanTargets, err := resolveRelativeTargets(s.FS, targets)
	if err != nil {
		return err
	}

	debug.Log("clean targets %v", cleanTargets)

	// we're using the same tree representation as the archiver does
	tree, err := newTree(s.FS, cleanTargets)
	if err != nil {
		return err
	}

	s.total = ScanStats{}
	s.walkers = nil
	if s.Concurrency > 1 {
		s.walkers = make(chan struct{}, s.Concurrency-1)
	}

	if err := s.scanTree(ctx, *tree); err != nil {
		return err
	}

	s.mu.Lock()
	stats := s.total
	s.mu.Unlock()

	s.Result("", stats)
	debug.Log("result: %+v", stats)
	return nil
}

// add counts an item and reports the new total. Reporting while holding the
// lock keeps the reported totals growing even when entries finish out of order.
func (s *Scanner) add(item string, delta ScanStats) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.total.Files += delta.Files
	s.total.Dirs += delta.Dirs
	s.total.Others += delta.Others
	s.total.Bytes += delta.Bytes
	s.Result(item, s.total)
}

// explicit is true when this path was an explicit backup target (same meaning as tree.Explicit on a leaf).
func (s *Scanner) scan(ctx context.Context, target string, explicit bool) error {
	if ctx.Err() != nil {
		return nil
	}

	// exclude files by path before running stat to reduce number of lstat calls
	if !explicit && !s.SelectByName(target) {
		return nil
	}

	// get file information
	fi, err := s.FS.Lstat(target)
	if err != nil {
		return s.Error(target, err)
	}

	// run remaining select functions that require file information
	if !explicit && !s.Select(target, fi, s.FS) {
		return nil
	}

	switch {
	case fi.Mode.IsRegular():
		s.add(target, ScanStats{Files: 1, Bytes: uint64(fi.Size)})
	case fi.Mode.IsDir():
		names, err := fs.Readdirnames(s.FS, target, fs.O_NOFOLLOW)
		if err != nil {
			return s.Error(target, err)
		}
		sort.Strings(names)

		if err := s.scanEntries(ctx, target, names); err != nil {
			return err
		}
		s.add(target, ScanStats{Dirs: 1})
	default:
		s.add(target, ScanStats{Others: 1})
	}

	return nil
}

// scanEntries scans the entries of a directory, handing them to free walkers
// and scanning them inline otherwise, and returns once all of them are done.
func (s *Scanner) scanEntries(ctx context.Context, dir string, names []string) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	scanEntry := func(pathname string) {
		err := s.scan(ctx, pathname, false)

		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}

	for _, name := range names {
		if failed() {
			break
		}

		pathname := s.FS.Join(dir, name)
		if !s.tryStartWalker() {
			scanEntry(pathname)
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.stopWalker()
			scanEntry(pathname)
		}()
	}

	wg.Wait()
	return firstErr
}

func (s *Scanner) tryStartWalker() bool {
	if s.walkers == nil {
		return false
	}
	select {
	case s.walkers <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Scanner) stopWalker() {
	<-s.walkers
}
