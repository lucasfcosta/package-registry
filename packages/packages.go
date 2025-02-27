// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package packages

import (
	"archive/zip"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/pkg/errors"
	"go.elastic.co/apm"

	"github.com/elastic/package-registry/util"
)

// ValidationDisabled is a flag which can disable package content validation (package, data streams, assets, etc.).
var ValidationDisabled bool

// Packages is a list of packages.
type Packages []*Package

func (p Packages) Len() int      { return len(p) }
func (p Packages) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
func (p Packages) Less(i, j int) bool {
	if p[i].Title != nil && p[j].Title != nil && *p[i].Title != *p[j].Title {
		return *p[i].Title < *p[j].Title
	}
	return p[i].Version < p[j].Version
}

// Join returns a set of packages that combines both sets.
func (p1 Packages) Join(p2 Packages) Packages {
	// TODO: Avoid duplications?
	return append(p1, p2...)
}

// GetOptions can be used to pass options to Get.
type GetOptions struct {
	// Filter to apply when querying for packages. If the filter is nil,
	// all packages are returned. This is different to a zero-object filter,
	// where internal and experimental packages are filtered by default.
	Filter *Filter
}

// FileSystemIndexer indexes packages from the filesystem.
type FileSystemIndexer struct {
	paths       []string
	packageList Packages

	// Label used for APM instrumentation.
	label string

	// Walker function used to look for files, it returns true for paths that should be indexed.
	walkerFn func(basePath, path string, info os.DirEntry) (shouldIndex bool, err error)

	// Builder to access the files of a package in this indexer.
	fsBuilder FileSystemBuilder
}

// NewFileSystemIndexer creates a new FileSystemIndexer for the given paths.
func NewFileSystemIndexer(paths ...string) *FileSystemIndexer {
	walkerFn := func(basePath, path string, info os.DirEntry) (bool, error) {
		relativePath, err := filepath.Rel(basePath, path)
		if err != nil {
			return false, err
		}

		dirs := strings.Split(relativePath, string(filepath.Separator))
		if len(dirs) < 2 {
			return false, nil // need to go to the package version level
		}

		if info.IsDir() {
			versionDir := dirs[1]
			_, err := semver.StrictNewVersion(versionDir)
			if err != nil {
				log.Printf("warning: unexpected directory: %s, ignoring", path)
				return false, filepath.SkipDir
			}
			return true, nil
		}
		// Unexpected file, return nil in order to continue processing sibling directories
		// Fixes an annoying problem when the .DS_Store file is left behind and the package
		// is not loading without any error information
		log.Printf("warning: unexpected file: %s, ignoring", path)
		return false, nil
	}
	fsBuilder := func(p *Package) (PackageFileSystem, error) {
		return NewExtractedPackageFileSystem(p)
	}
	return &FileSystemIndexer{
		paths:     paths,
		label:     "FileSystemIndexer",
		walkerFn:  walkerFn,
		fsBuilder: fsBuilder,
	}
}

// NewZipFileSystemIndexer creates a new ZipFileSystemIndexer for the given paths.
func NewZipFileSystemIndexer(paths ...string) *FileSystemIndexer {
	walkerFn := func(basePath, path string, info os.DirEntry) (bool, error) {
		if info.IsDir() {
			return false, nil
		}
		if !strings.HasSuffix(path, ".zip") {
			return false, nil
		}

		// Check if the file is actually a zip file.
		r, err := zip.OpenReader(path)
		if err != nil {
			log.Printf("warning: zip file cannot be opened as zip: %s, ignoring: %v", path, err)
			return false, nil
		}
		defer r.Close()

		return true, nil
	}
	fsBuilder := func(p *Package) (PackageFileSystem, error) {
		return NewZipPackageFileSystem(p)
	}
	return &FileSystemIndexer{
		paths:     paths,
		label:     "ZipFileSystemIndexer",
		walkerFn:  walkerFn,
		fsBuilder: fsBuilder,
	}
}

// Init initializes the indexer.
func (i *FileSystemIndexer) Init(ctx context.Context) (err error) {
	i.packageList, err = i.getPackagesFromFileSystem(ctx)
	if err != nil {
		return errors.Wrapf(err, "reading packages from filesystem failed")
	}
	return nil
}

// Get returns a slice with packages.
// Options can be used to filter the returned list of packages. When no options are passed
// or they don't contain any filter, no filtering is done.
// The list is stored in memory and on the second request directly served from memory.
// This assumes changes to packages only happen on restart (unless development mode is enabled).
// Caching the packages request many file reads every time this method is called.
func (i *FileSystemIndexer) Get(ctx context.Context, opts *GetOptions) (Packages, error) {
	if opts == nil {
		return i.packageList, nil
	}

	if opts.Filter != nil {
		return opts.Filter.Apply(ctx, i.packageList), nil
	}

	return i.packageList, nil
}

func (i *FileSystemIndexer) getPackagesFromFileSystem(ctx context.Context) (Packages, error) {
	span, ctx := apm.StartSpan(ctx, "GetFromFileSystem", "app")
	span.Context.SetLabel("indexer", i.label)
	defer span.End()

	type packageKey struct {
		name    string
		version string
	}
	packagesFound := make(map[packageKey]struct{})

	var pList Packages
	for _, basePath := range i.paths {
		packagePaths, err := i.getPackagePaths(basePath)
		if err != nil {
			return nil, err
		}

		log.Printf("Packages in %s:", basePath)
		for _, path := range packagePaths {
			p, err := NewPackage(path, i.fsBuilder)
			if err != nil {
				return nil, errors.Wrapf(err, "loading package failed (path: %s)", path)
			}

			key := packageKey{name: p.Name, version: p.Version}
			if _, found := packagesFound[key]; found {
				log.Printf("%-20s\t%10s\t%s", p.Name+" (duplicated)", p.Version, p.BasePath)
				continue
			}

			packagesFound[key] = struct{}{}
			pList = append(pList, p)

			log.Printf("%-20s\t%10s\t%s", p.Name, p.Version, p.BasePath)
		}
	}
	return pList, nil
}

// getPackagePaths returns list of available packages, one for each version.
func (i *FileSystemIndexer) getPackagePaths(packagesPath string) ([]string, error) {
	var foundPaths []string
	err := filepath.WalkDir(packagesPath, func(path string, info os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}

		shouldIndex, err := i.walkerFn(packagesPath, path, info)
		if err != nil {
			return err
		}
		if !shouldIndex {
			return nil
		}
		foundPaths = append(foundPaths, path)
		if info.IsDir() {
			// If a directory is being added, consider all its contents part of
			// the package and continue.
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "listing packages failed (path: %s)", packagesPath)
	}
	return foundPaths, nil
}

// Filter can be used to filter a list of packages.
type Filter struct {
	AllVersions    bool
	Category       string
	Experimental   bool
	Internal       bool
	KibanaVersion  *semver.Version
	PackageName    string
	PackageVersion string
}

// Apply applies the filter to the list of packages, if the filter is nil, no filtering is done.
func (f *Filter) Apply(ctx context.Context, packages Packages) Packages {
	if f == nil {
		return packages
	}

	span, ctx := apm.StartSpan(ctx, "FilterPackages", "app")
	defer span.End()

	// Checks that only the most recent version of an integration is added to the list
	var packagesList Packages
	for _, p := range packages {
		// Skip internal packages by default
		if p.Internal && !f.Internal {
			continue
		}

		// Skip experimental packages if flag is not specified
		if p.Release == ReleaseExperimental && !f.Experimental {
			continue
		}

		if f.KibanaVersion != nil {
			if valid := p.HasKibanaVersion(f.KibanaVersion); !valid {
				continue
			}
		}

		if f.PackageName != "" && f.PackageName != p.Name {
			continue
		}

		if f.PackageVersion != "" && f.PackageVersion != p.Version {
			continue
		}

		addPackage := true
		if !f.AllVersions {
			// Check if the version exists and if it should be added or not.
			for i, current := range packagesList {
				if current.Name != p.Name {
					continue
				}

				addPackage = false

				// If the package in the list is newer or equal, do nothing.
				if current.IsNewerOrEqual(p) {
					continue
				}

				// Otherwise replace it.
				packagesList[i] = p
			}
		}

		if addPackage {
			packagesList = append(packagesList, p)
		}
	}

	// Filter by category after selecting the newer packages.
	packagesList = filterCategories(packagesList, f.Category)

	return packagesList
}

func filterCategories(packages Packages, category string) Packages {
	if category == "" {
		return packages
	}
	var result Packages
	for _, p := range packages {
		if !p.HasCategory(category) && !p.HasPolicyTemplateWithCategory(category) {
			continue
		}
		if !p.HasCategory(category) {
			p = filterPolicyTemplates(*p, category)
		}

		result = append(result, p)
	}
	return result
}

func filterPolicyTemplates(p Package, category string) *Package {
	var updatedPolicyTemplates []PolicyTemplate
	var updatedBasePolicyTemplates []BasePolicyTemplate
	for i, pt := range p.PolicyTemplates {
		if util.StringsContains(pt.Categories, category) {
			updatedPolicyTemplates = append(updatedPolicyTemplates, pt)
			updatedBasePolicyTemplates = append(updatedBasePolicyTemplates, p.BasePackage.BasePolicyTemplates[i])
		}
	}
	p.PolicyTemplates = updatedPolicyTemplates
	p.BasePackage.BasePolicyTemplates = updatedBasePolicyTemplates
	return &p
}

// NameVersionFilter is a helper to initialize a Filter with the usual
// options to look per name and version along all packages indexed.
func NameVersionFilter(name, version string) GetOptions {
	return GetOptions{
		Filter: &Filter{
			Experimental:   true,
			Internal:       true,
			PackageName:    name,
			PackageVersion: version,
		},
	}
}
