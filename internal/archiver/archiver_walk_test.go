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

	"github.com/restic/restic/internal/checker"
	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/fs"
	rtest "github.com/restic/restic/internal/test"
)

func walkTestDir() TestDir {
	dir := TestDir{}
	for i := 0; i < 4; i++ {
		sub := TestDir{"empty": TestDir{}}
		for j := 0; j < 3; j++ {
			leaf := TestDir{}
			for k := 0; k < 20; k++ {
				leaf[fmt.Sprintf("file%02d", k)] = TestFile{Content: fmt.Sprintf("content %d %d %d", i, j, k)}
			}
			sub[fmt.Sprintf("leaf%d", j)] = leaf
		}
		dir[fmt.Sprintf("dir%d", i)] = sub
	}
	return dir
}

func TestArchiverWalkConcurrencyKeepsTrees(t *testing.T) {
	src := walkTestDir()
	tempdir, repo := prepareTempdirRepoSrc(t, src)
	back := rtest.Chdir(t, tempdir)
	defer back()

	backup := func(walkers uint, parent *data.Snapshot) (*data.Snapshot, *Summary) {
		arch := New(repo, fs.Track{FS: fs.Local{}}, Options{WalkConcurrency: walkers})
		arch.Error = func(item string, err error) error {
			t.Errorf("archiver error for %v: %v", item, err)
			return err
		}
		sn, _, summary, err := arch.Snapshot(context.Background(), []string{"."}, SnapshotOptions{Time: time.Now(), ParentSnapshot: parent})
		rtest.OK(t, err)
		return sn, summary
	}

	serial, _ := backup(1, nil)
	parallel, _ := backup(16, nil)
	rtest.Equals(t, *serial.Tree, *parallel.Tree)

	incremental, summary := backup(16, serial)
	rtest.Equals(t, *serial.Tree, *incremental.Tree)
	rtest.Equals(t, ChangeStats{Unchanged: 240}, summary.Files)
	rtest.Equals(t, ChangeStats{Unchanged: 20}, summary.Dirs)
	rtest.Equals(t, 0, summary.DataBlobs)
	rtest.Equals(t, 0, summary.TreeBlobs)

	checker.TestCheckRepo(t, repo)
}

// barrierFS holds the metadata lookup of every file until want lookups are in
// progress at the same time, or until a timeout passes.
type barrierFS struct {
	fs.FS
	want    int32
	arrived atomic.Int32
	reached atomic.Bool
	release chan struct{}
	once    sync.Once
}

func (b *barrierFS) OpenFile(name string, flag int, metadataOnly bool) (fs.File, error) {
	if metadataOnly && strings.HasPrefix(filepath.Base(name), "file") {
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
	return b.FS.OpenFile(name, flag, metadataOnly)
}

func TestArchiverWalkConcurrencyInspectsEntriesConcurrently(t *testing.T) {
	files := TestDir{}
	for i := 0; i < 16; i++ {
		files[fmt.Sprintf("file%02d", i)] = TestFile{Content: fmt.Sprintf("content %d", i)}
	}
	tempdir, repo := prepareTempdirRepoSrc(t, TestDir{"dir": files})
	back := rtest.Chdir(t, tempdir)
	defer back()

	testFS := &barrierFS{FS: fs.Track{FS: fs.Local{}}, want: 4, release: make(chan struct{})}
	arch := New(repo, testFS, Options{WalkConcurrency: 8})
	_, _, _, err := arch.Snapshot(context.Background(), []string{"."}, SnapshotOptions{Time: time.Now()})
	rtest.OK(t, err)

	rtest.Assert(t, testFS.reached.Load(), "expected %d files to be inspected at the same time", testFS.want)
}

// closeTrackFS counts the files that were opened and those still open. Opening
// a file takes delay, so that inspections are still running when the backup
// is aborted.
type closeTrackFS struct {
	fs.FS
	delay  time.Duration
	mu     sync.Mutex
	opened int
	open   int
}

func (c *closeTrackFS) OpenFile(name string, flag int, metadataOnly bool) (fs.File, error) {
	if strings.HasPrefix(filepath.Base(name), "file") {
		time.Sleep(c.delay)
	}
	f, err := c.FS.OpenFile(name, flag, metadataOnly)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.opened++
	c.open++
	c.mu.Unlock()
	return &closeTrackFile{File: f, fs: c}, nil
}

func (c *closeTrackFS) counts() (opened, open int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opened, c.open
}

type closeTrackFile struct {
	fs.File
	fs *closeTrackFS
}

func (f *closeTrackFile) Close() error {
	f.fs.mu.Lock()
	f.fs.open--
	f.fs.mu.Unlock()
	return f.File.Close()
}

func TestArchiverAbortEarlyOnErrorWithWalkers(t *testing.T) {
	files := TestDir{}
	for i := 0; i < 32; i++ {
		files[fmt.Sprintf("file%02d", i)] = TestFile{Content: string(rtest.Random(i, 1024))}
	}
	tempdir, repo := prepareTempdirRepoSrc(t, TestDir{"dir": files})
	back := rtest.Chdir(t, tempdir)
	defer back()

	testFS := &closeTrackFS{FS: fs.Track{FS: fs.Local{}}, delay: 20 * time.Millisecond}
	testErr := context.Canceled
	testRepo := &failSaveRepo{archiverRepo: repo, failAfter: 4, err: testErr}

	arch := New(testRepo, testFS, Options{ReadConcurrency: 2, WalkConcurrency: 4})
	_, _, _, err := arch.Snapshot(context.Background(), []string{"."}, SnapshotOptions{Time: time.Now()})
	if !errors.Is(err, testErr) {
		t.Fatalf("expected error (%v) not found, got %v", testErr, err)
	}

	// Walkers that were already inspecting entries must have stopped before
	// Snapshot returned, and closed every file they opened.
	opened, open := testFS.counts()
	rtest.Equals(t, 0, open)
	time.Sleep(100 * time.Millisecond)
	openedLater, _ := testFS.counts()
	rtest.Equals(t, opened, openedLater)
}
