package archiver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/restic/restic/internal/fs"
	rtest "github.com/restic/restic/internal/test"
)

func TestScannerWalkConcurrencyMatchesSequential(t *testing.T) {
	tempdir := rtest.TempDir(t)
	TestCreateFiles(t, tempdir, walkTestDir())
	back := rtest.Chdir(t, tempdir)
	defer back()

	scan := func(walkers uint) (map[string]struct{}, ScanStats) {
		sc := NewScanner(fs.Track{FS: fs.Local{}})
		sc.Concurrency = walkers

		var mu sync.Mutex
		items := make(map[string]struct{})
		var last ScanStats
		sc.Result = func(item string, s ScanStats) {
			mu.Lock()
			defer mu.Unlock()
			if s.Files < last.Files || s.Dirs < last.Dirs || s.Others < last.Others || s.Bytes < last.Bytes {
				t.Errorf("totals went down from %+v to %+v", last, s)
			}
			last = s
			items[item] = struct{}{}
		}

		rtest.OK(t, sc.Scan(context.Background(), []string{"."}))
		return items, last
	}

	serialItems, serialTotal := scan(1)
	parallelItems, parallelTotal := scan(16)
	rtest.Equals(t, ScanStats{Files: 240, Dirs: 20, Bytes: serialTotal.Bytes}, serialTotal)
	rtest.Equals(t, serialTotal, parallelTotal)
	rtest.Equals(t, serialItems, parallelItems)
}

// lstatBarrierFS holds the lstat of every file until want of them are in
// progress at the same time, or until a timeout passes.
type lstatBarrierFS struct {
	fs.FS
	want    int32
	arrived atomic.Int32
	reached atomic.Bool
	release chan struct{}
	once    sync.Once
}

func (b *lstatBarrierFS) Lstat(name string) (*fs.ExtendedFileInfo, error) {
	if strings.HasPrefix(filepath.Base(name), "file") {
		if b.arrived.Add(1) == b.want {
			b.once.Do(func() {
				b.reached.Store(true)
				close(b.release)
			})
		}
		select {
		case <-b.release:
		case <-time.After(10 * time.Second):
			b.once.Do(func() { close(b.release) })
		}
	}
	return b.FS.Lstat(name)
}

func TestScannerWalkConcurrencyScansEntriesConcurrently(t *testing.T) {
	files := TestDir{}
	for i := 0; i < 16; i++ {
		files[fmt.Sprintf("file%02d", i)] = TestFile{Content: fmt.Sprintf("content %d", i)}
	}
	tempdir := rtest.TempDir(t)
	TestCreateFiles(t, tempdir, TestDir{"dir": files})
	back := rtest.Chdir(t, tempdir)
	defer back()

	testFS := &lstatBarrierFS{FS: fs.Track{FS: fs.Local{}}, want: 4, release: make(chan struct{})}
	sc := NewScanner(testFS)
	sc.Concurrency = 8
	rtest.OK(t, sc.Scan(context.Background(), []string{"."}))

	rtest.Assert(t, testFS.reached.Load(), "expected %d files to be scanned at the same time", testFS.want)
}

// slowLstatFS counts the lstat of every file, fails the one named fail at once
// and delays all others.
type slowLstatFS struct {
	fs.FS
	delay  time.Duration
	fail   string
	lstats atomic.Int32
}

func (s *slowLstatFS) Lstat(name string) (*fs.ExtendedFileInfo, error) {
	if strings.HasPrefix(filepath.Base(name), "file") {
		s.lstats.Add(1)
		if filepath.Base(name) == s.fail {
			return nil, errors.New("lstat failed")
		}
		time.Sleep(s.delay)
	}
	return s.FS.Lstat(name)
}

func TestScannerStopsWalkersOnError(t *testing.T) {
	files := TestDir{}
	for i := 0; i < 32; i++ {
		files[fmt.Sprintf("file%02d", i)] = TestFile{Content: fmt.Sprintf("content %d", i)}
	}
	tempdir := rtest.TempDir(t)
	TestCreateFiles(t, tempdir, TestDir{"dir": files})
	back := rtest.Chdir(t, tempdir)
	defer back()

	// The three walkers take file00 to file02, so the scan of file03 runs inline
	// and fails while they are still busy.
	testFS := &slowLstatFS{FS: fs.Track{FS: fs.Local{}}, delay: 100 * time.Millisecond, fail: "file03"}
	testErr := errors.New("scan aborted")

	var returned atomic.Bool
	sc := NewScanner(testFS)
	sc.Concurrency = 4
	sc.Error = func(item string, err error) error { return testErr }
	sc.Result = func(item string, s ScanStats) {
		if returned.Load() {
			t.Errorf("result for %v reported after Scan returned", item)
		}
	}

	err := sc.Scan(context.Background(), []string{"."})
	returned.Store(true)
	rtest.Assert(t, errors.Is(err, testErr), "expected error %v, got %v", testErr, err)

	lstats := testFS.lstats.Load()
	rtest.Assert(t, lstats < 32, "expected the scan to stop early, got %d lstats", lstats)
	time.Sleep(200 * time.Millisecond)
	rtest.Equals(t, lstats, testFS.lstats.Load())
}
