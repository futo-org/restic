package main

import (
	"encoding/json"
	"path/filepath"
	"slices"

	"github.com/restic/restic/internal/archiver"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/filter"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/repository"
)

// rejectResticCache returns a RejectByNameFunc that rejects the restic cache
// directory (if set).
func rejectResticCache(repo *repository.Repository) (archiver.RejectByNameFunc, error) {
	if repo.Cache() == nil {
		return func(string) bool {
			return false
		}, nil
	}
	cacheBase := repo.Cache().BaseDir()

	if cacheBase == "" {
		return nil, errors.New("cacheBase is empty string")
	}

	return func(item string) bool {
		if fs.HasPathPrefix(cacheBase, item) {
			debug.Log("rejecting restic cache directory %v", item)
			return true
		}

		return false
	}, nil
}

type globScope struct {
	Paths    []string `json:"paths"`
	Patterns []string `json:"patterns"`
}

// rejectByScopedGlob returns a RejectByNameFunc for one scope, given as JSON.
// It rejects a path under one of the scope's paths that matches one of its
// patterns.
func rejectByScopedGlob(spec string) (archiver.RejectByNameFunc, error) {
	var scope globScope
	err := json.Unmarshal([]byte(spec), &scope)
	if err != nil {
		return nil, errors.Fatalf("--iexclude-scoped-glob: %v", err)
	}

	for _, path := range scope.Paths {
		if !filepath.IsAbs(path) {
			return nil, errors.Fatalf("--iexclude-scoped-glob: path %q is not absolute", path)
		}
	}

	globs := make([]filter.Glob, 0, len(scope.Patterns))
	for _, pattern := range scope.Patterns {
		glob, err := filter.ParseGlob(pattern)
		if err != nil {
			return nil, errors.Fatalf("--iexclude-scoped-glob: Invalid exclusion pattern '%s': %v", pattern, err)
		}

		globs = append(globs, glob)
	}

	return func(item string) bool {
		inScope := slices.ContainsFunc(scope.Paths, func(path string) bool {
			return fs.HasPathPrefix(path, item)
		})
		if !inScope {
			return false
		}

		for _, glob := range globs {
			if glob.Match(item) {
				debug.Log("path %q excluded by a scoped glob pattern", item)
				return true
			}
		}

		return false
	}, nil
}
