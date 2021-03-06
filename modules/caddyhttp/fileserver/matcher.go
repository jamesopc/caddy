// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fileserver

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(MatchFile{})
}

// MatchFile is an HTTP request matcher that can match
// requests based upon file existence.
//
// Upon matching, two new placeholders will be made
// available:
//
// - `{http.matchers.file.relative}` The root-relative
// path of the file. This is often useful when rewriting
// requests.
// - `{http.matchers.file.absolute}` The absolute path
// of the matched file.
type MatchFile struct {
	// The root directory, used for creating absolute
	// file paths, and required when working with
	// relative paths; if not specified, `{http.vars.root}`
	// will be used, if set; otherwise, the current
	// directory is assumed. Accepts placeholders.
	Root string `json:"root,omitempty"`

	// The list of files to try. Each path here is
	// considered related to Root. If nil, the request
	// URL's path will be assumed. Files and
	// directories are treated distinctly, so to match
	// a directory, the filepath MUST end in a forward
	// slash `/`. To match a regular file, there must
	// be no trailing slash. Accepts placeholders.
	TryFiles []string `json:"try_files,omitempty"`

	// How to choose a file in TryFiles. Can be:
	//
	// - first_exist
	// - smallest_size
	// - largest_size
	// - most_recently_modified
	//
	// Default is first_exist.
	TryPolicy string `json:"try_policy,omitempty"`

	// A list of delimiters to use to split the path in two
	// when trying files. If empty, no splitting will
	// occur, and the path will be tried as-is. For each
	// split value, the left-hand side of the split,
	// including the split value, will be the path tried.
	// For example, the path `/remote.php/dav/` using the
	// split value `.php` would try the file `/remote.php`.
	// Each delimiter must appear at the end of a URI path
	// component in order to be used as a split delimiter.
	SplitPath []string `json:"split_path,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (MatchFile) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.file",
		New: func() caddy.Module { return new(MatchFile) },
	}
}

// UnmarshalCaddyfile sets up the matcher from Caddyfile tokens. Syntax:
//
//     file {
//         root <path>
//         try_files <files...>
//         try_policy first_exist|smallest_size|largest_size|most_recently_modified
//     }
//
func (m *MatchFile) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "root":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Root = d.Val()
			case "try_files":
				m.TryFiles = d.RemainingArgs()
				if len(m.TryFiles) == 0 {
					return d.ArgErr()
				}
			case "try_policy":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.TryPolicy = d.Val()
			case "split":
				m.SplitPath = d.RemainingArgs()
				if len(m.SplitPath) == 0 {
					return d.ArgErr()
				}
			}
		}
	}
	return nil
}

// Provision sets up m's defaults.
func (m *MatchFile) Provision(_ caddy.Context) error {
	if m.Root == "" {
		m.Root = "{http.vars.root}"
	}
	return nil
}

// Validate ensures m has a valid configuration.
func (m MatchFile) Validate() error {
	switch m.TryPolicy {
	case "",
		tryPolicyFirstExist,
		tryPolicyLargestSize,
		tryPolicySmallestSize,
		tryPolicyMostRecentlyMod:
	default:
		return fmt.Errorf("unknown try policy %s", m.TryPolicy)
	}
	return nil
}

// Match returns true if r matches m. Returns true
// if a file was matched. If so, two placeholders
// will be available:
//    - http.matchers.file.relative
//    - http.matchers.file.absolute
func (m MatchFile) Match(r *http.Request) bool {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	rel, abs, matched := m.selectFile(r)
	if matched {
		repl.Set("http.matchers.file.relative", rel)
		repl.Set("http.matchers.file.absolute", abs)
	}
	return matched
}

// selectFile chooses a file according to m.TryPolicy by appending
// the paths in m.TryFiles to m.Root, with placeholder replacements.
// It returns the root-relative path to the matched file, the full
// or absolute path, and whether a match was made.
func (m MatchFile) selectFile(r *http.Request) (rel, abs string, matched bool) {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	root := repl.ReplaceAll(m.Root, ".")

	// if list of files to try was omitted entirely,
	// assume URL path
	if m.TryFiles == nil {
		// m is not a pointer, so this is safe
		m.TryFiles = []string{r.URL.Path}
	}

	switch m.TryPolicy {
	case "", tryPolicyFirstExist:
		for _, f := range m.TryFiles {
			suffix := m.firstSplit(path.Clean(repl.ReplaceAll(f, "")))
			fullpath := sanitizedPathJoin(root, suffix)
			if strictFileExists(fullpath) {
				return suffix, fullpath, true
			}
		}

	case tryPolicyLargestSize:
		var largestSize int64
		var largestFilename string
		var largestSuffix string
		for _, f := range m.TryFiles {
			suffix := m.firstSplit(path.Clean(repl.ReplaceAll(f, "")))
			fullpath := sanitizedPathJoin(root, suffix)
			info, err := os.Stat(fullpath)
			if err == nil && info.Size() > largestSize {
				largestSize = info.Size()
				largestFilename = fullpath
				largestSuffix = suffix
			}
		}
		return largestSuffix, largestFilename, true

	case tryPolicySmallestSize:
		var smallestSize int64
		var smallestFilename string
		var smallestSuffix string
		for _, f := range m.TryFiles {
			suffix := m.firstSplit(path.Clean(repl.ReplaceAll(f, "")))
			fullpath := sanitizedPathJoin(root, suffix)
			info, err := os.Stat(fullpath)
			if err == nil && (smallestSize == 0 || info.Size() < smallestSize) {
				smallestSize = info.Size()
				smallestFilename = fullpath
				smallestSuffix = suffix
			}
		}
		return smallestSuffix, smallestFilename, true

	case tryPolicyMostRecentlyMod:
		var recentDate time.Time
		var recentFilename string
		var recentSuffix string
		for _, f := range m.TryFiles {
			suffix := m.firstSplit(path.Clean(repl.ReplaceAll(f, "")))
			fullpath := sanitizedPathJoin(root, suffix)
			info, err := os.Stat(fullpath)
			if err == nil &&
				(recentDate.IsZero() || info.ModTime().After(recentDate)) {
				recentDate = info.ModTime()
				recentFilename = fullpath
				recentSuffix = suffix
			}
		}
		return recentSuffix, recentFilename, true
	}

	return
}

// strictFileExists returns true if file exists
// and matches the convention of the given file
// path. If the path ends in a forward slash,
// the file must also be a directory; if it does
// NOT end in a forward slash, the file must NOT
// be a directory.
func strictFileExists(file string) bool {
	stat, err := os.Stat(file)
	if err != nil {
		// in reality, this can be any error
		// such as permission or even obscure
		// ones like "is not a directory" (when
		// trying to stat a file within a file);
		// in those cases we can't be sure if
		// the file exists, so we just treat any
		// error as if it does not exist; see
		// https://stackoverflow.com/a/12518877/1048862
		return false
	}
	if strings.HasSuffix(file, "/") {
		// by convention, file paths ending
		// in a slash must be a directory
		return stat.IsDir()
	}
	// by convention, file paths NOT ending
	// in a slash must NOT be a directory
	return !stat.IsDir()
}

// firstSplit returns the first result where the path
// can be split in two by a value in m.SplitPath. The
// result is the first piece of the path that ends with
// in the split value. Returns the path as-is if the
// path cannot be split.
func (m MatchFile) firstSplit(path string) string {
	lowerPath := strings.ToLower(path)
	for _, split := range m.SplitPath {
		if idx := strings.Index(lowerPath, strings.ToLower(split)); idx > -1 {
			pos := idx + len(split)
			// skip the split if it's not the final part of the filename
			if pos != len(path) && !strings.HasPrefix(path[pos:], "/") {
				continue
			}
			return path[:pos]
		}
	}
	return path
}

const (
	tryPolicyFirstExist      = "first_exist"
	tryPolicyLargestSize     = "largest_size"
	tryPolicySmallestSize    = "smallest_size"
	tryPolicyMostRecentlyMod = "most_recently_modified"
)

// Interface guards
var (
	_ caddy.Validator          = (*MatchFile)(nil)
	_ caddyhttp.RequestMatcher = (*MatchFile)(nil)
)
