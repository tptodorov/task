package fingerprint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	gitignore "github.com/sabhiram/go-gitignore"
	"github.com/go-task/task/v3/internal/execext"
	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/taskfile/ast"
)

// globDebug enables debug logging for glob operations
var globDebug = os.Getenv("TASK_GLOB_DEBUG") != ""

func Globs(dir string, globs []*ast.Glob) ([]string, error) {
	return GlobsWithIgnore(dir, globs, true)
}

func GlobsWithIgnore(dir string, globs []*ast.Glob, respectGitignore bool) ([]string, error) {
	resultMap := make(map[string]bool)
	for _, g := range globs {
		matches, err := glob(dir, g.Glob, respectGitignore)
		if err != nil {
			continue
		}
		for _, match := range matches {
			resultMap[match] = !g.Negate
		}
	}
	return collectKeys(resultMap), nil
}

func glob(dir string, g string, respectGitignore bool) ([]string, error) {
	start := time.Now()
	originalPattern := g
	g = filepathext.SmartJoin(dir, g)
	
	if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOB] Starting glob: dir=%q, pattern=%q, joined=%q, respectGitignore=%v\n", dir, originalPattern, g, respectGitignore)
	}

	// Check if the pattern contains shell features that require ExpandFields
	if hasShellFeatures(g) {
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOB] Using shell expansion for pattern: %q\n", g)
		}
		shellStart := time.Now()
		// Fall back to shell expansion for complex patterns
		fs, err := execext.ExpandFields(g)
		if err != nil {
			return nil, err
		}
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOB] Shell expansion took %v, found %d files\n", time.Since(shellStart), len(fs))
		}

		results := make(map[string]bool, len(fs))
		for _, f := range fs {
			info, err := os.Stat(f)
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				continue
			}
			results[f] = true
		}
		result := collectKeys(results)
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOB] Shell expansion total time: %v, result count: %d\n", time.Since(start), len(result))
		}
		return result, nil
	}

	// Use doublestar for simple glob patterns (much faster than shell expansion)
	// For patterns with **, use filepath.Walk with optional .gitignore support
	if strings.Contains(g, "**") {
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOB] Using globstarWalk for pattern: %q\n", g)
		}
		result, err := globstarWalk(dir, g, respectGitignore)
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOB] globstarWalk took %v, found %d files\n", time.Since(start), len(result))
		}
		return result, err
	}
	
	if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOB] Using doublestar.Glob for pattern: %q\n", g)
	}
	
	// For non-globstar patterns, use doublestar.Glob
	relPattern, err := filepath.Rel(dir, g)
	if err != nil {
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOB] Relative path calculation failed, using FilepathGlob fallback\n")
		}
		fallbackStart := time.Now()
		// Fallback to FilepathGlob if relative path calculation fails
		matches, err := doublestar.FilepathGlob(g, doublestar.WithFilesOnly())
		if err != nil {
			return nil, err
		}
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOB] FilepathGlob took %v, found %d files\n", time.Since(fallbackStart), len(matches))
		}
		if respectGitignore {
			filterStart := time.Now()
			result := filterIgnoredPaths(dir, matches)
			if globDebug {
				fmt.Fprintf(os.Stderr, "[GLOB] Filtering took %v, filtered to %d files\n", time.Since(filterStart), len(result))
			}
			return result, nil
		}
		return matches, nil
	}
	
	// Create filesystem from base directory
	fsysStart := time.Now()
	fsys := os.DirFS(dir)
	
	// Wrap with filesystem that filters out ignored paths (only if respectGitignore is true)
	if respectGitignore {
		fsys = &gitignoreFS{fsys: fsys, baseDir: dir, respectGitignore: true}
	}
	
	// Use forward slashes for pattern (doublestar convention)
	pattern := filepath.ToSlash(relPattern)
	
	if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOB] Filesystem setup took %v, pattern=%q, respectGitignore=%v\n", time.Since(fsysStart), pattern, respectGitignore)
	}
	
	globStart := time.Now()
	matches, err := doublestar.Glob(fsys, pattern, doublestar.WithFilesOnly())
	if err != nil {
		return nil, err
	}
	
	if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOB] doublestar.Glob took %v, found %d matches\n", time.Since(globStart), len(matches))
	}
	
	// Convert relative paths back to absolute paths
	convertStart := time.Now()
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		fullPath := filepath.Join(dir, match)
		result = append(result, filepath.Clean(fullPath))
	}
	
	if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOB] Path conversion took %v\n", time.Since(convertStart))
		fmt.Fprintf(os.Stderr, "[GLOB] Total time: %v, result count: %d\n", time.Since(start), len(result))
	}
	
	return result, nil
}

// filterIgnoredPaths filters out ignored paths from a list
func filterIgnoredPaths(dir string, paths []string) []string {
	gi, _ := loadGitIgnore(dir)
	filtered := make([]string, 0, len(paths))
	for _, path := range paths {
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			continue
		}
		relPathSlash := filepath.ToSlash(relPath)
		
		if isBasicIgnoredPath(relPathSlash) {
			continue
		}
		
		if gi != nil && gi.MatchesPath(relPathSlash) {
			continue
		}
		
		filtered = append(filtered, path)
	}
	return filtered
}

// globstarWalk efficiently walks the directory tree, optionally skipping ignored directories
func globstarWalk(baseDir, pattern string, respectGitignore bool) ([]string, error) {
	walkStart := time.Now()
	
	// Load gitignore once for the entire walk (only if respecting it)
	var gi *gitignore.GitIgnore
	if respectGitignore {
		giStart := time.Now()
		gi, _ = loadGitIgnore(baseDir)
		if globDebug {
			fmt.Fprintf(os.Stderr, "[GLOBSTAR] Loading gitignore took %v\n", time.Since(giStart))
		}
	} else if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOBSTAR] Skipping .gitignore (respectGitignore=false)\n")
	}

	// Extract the base directory from the pattern
	// For patterns like "api/build/**/*.json", we need to find the directory before "**"
	// filepath.Dir would give us "api/build/**" which is wrong
	patternSlash := filepath.ToSlash(pattern)
	globstarIdx := strings.Index(patternSlash, "**")
	
	var basePath string
	var explicitPathPrefix string // The explicit path prefix that should not be skipped
	if globstarIdx >= 0 {
		// Find the directory containing the **
		beforeGlobstar := patternSlash[:globstarIdx]
		// Remove trailing slashes
		beforeGlobstar = strings.TrimSuffix(beforeGlobstar, "/")
		if beforeGlobstar == "" {
			// Pattern starts with **, walk from baseDir
			basePath = baseDir
			explicitPathPrefix = ""
		} else {
			// The beforeGlobstar is the directory we want to start walking from
			// For "/Users/.../api/build", we want "/Users/.../api/build"
			// For "api/build", we want to join it with baseDir
			if filepath.IsAbs(beforeGlobstar) {
				basePath = beforeGlobstar
				// Get relative path for prefix checking
				relPrefix, err := filepath.Rel(baseDir, beforeGlobstar)
				if err == nil {
					explicitPathPrefix = filepath.ToSlash(relPrefix)
				}
			} else {
				basePath = filepath.Join(baseDir, beforeGlobstar)
				explicitPathPrefix = beforeGlobstar
			}
		}
	} else {
		// Fallback: no ** found, use filepath.Dir
		basePath = filepath.Dir(pattern)
		if basePath == "." || basePath == "" {
			basePath = baseDir
			explicitPathPrefix = ""
		} else if !filepath.IsAbs(basePath) {
			basePath = filepath.Join(baseDir, basePath)
			explicitPathPrefix = filepath.Dir(pattern)
		} else {
			relPrefix, err := filepath.Rel(baseDir, basePath)
			if err == nil {
				explicitPathPrefix = filepath.ToSlash(relPrefix)
			}
		}
	}

	// Get relative pattern for matching (relative to baseDir, not basePath)
	relPattern, err := filepath.Rel(baseDir, pattern)
	if err != nil {
		relPattern = pattern
	}
	relPattern = filepath.ToSlash(relPattern)

	if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOBSTAR] Starting walk: baseDir=%q, basePath=%q, explicitPathPrefix=%q, pattern=%q\n", baseDir, basePath, explicitPathPrefix, relPattern)
	}

	results := make([]string, 0)
	walkedDirs := 0
	walkedFiles := 0
	skippedDirs := 0
	skippedFiles := 0
	matchedFiles := 0
	
	err = filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				skippedDirs++
				return filepath.SkipDir
			}
			return nil
		}

		// Get relative path once
		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			return nil
		}
		relPathSlash := filepath.ToSlash(relPath)

		// Skip ignored paths only if respectGitignore is true
		shouldSkip := false
		if respectGitignore {
			// Don't skip paths that are part of the explicit path prefix in the pattern
			// For example, if pattern is "api/build/**/*.json", don't skip "api/build" or its subdirs
			if explicitPathPrefix != "" && strings.HasPrefix(relPathSlash, explicitPathPrefix+"/") {
				// This path is under the explicit prefix, don't skip it even if in .gitignore
				shouldSkip = false
			} else {
				// Quick check for common ignored directories first
				if isBasicIgnoredPath(relPathSlash) {
					shouldSkip = true
				} else if gi != nil && gi.MatchesPath(relPathSlash) {
					// Check gitignore using pre-loaded matcher
					shouldSkip = true
				}
			}
		}
		
		if shouldSkip {
			if info.IsDir() {
				skippedDirs++
				if globDebug {
					fmt.Fprintf(os.Stderr, "[GLOBSTAR] Skipped dir: %q\n", relPathSlash)
				}
				return filepath.SkipDir
			}
			skippedFiles++
			if globDebug {
				fmt.Fprintf(os.Stderr, "[GLOBSTAR] Skipped file: %q\n", relPathSlash)
			}
			return nil
		}

		// Only process files
		if info.IsDir() {
			walkedDirs++
			if globDebug && walkedDirs%1000 == 0 {
				fmt.Fprintf(os.Stderr, "[GLOBSTAR] Walked %d dirs, skipped %d dirs, processed %d files, matched %d files\n", 
					walkedDirs, skippedDirs, walkedFiles, matchedFiles)
			}
			return nil
		}

		walkedFiles++

		// Check if file matches the pattern
		matched, err := doublestar.Match(relPattern, relPathSlash)
		if err != nil {
			return nil
		}

		if matched {
			matchedFiles++
			results = append(results, path)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if globDebug {
		fmt.Fprintf(os.Stderr, "[GLOBSTAR] Walk complete: walked %d dirs, %d files; skipped %d dirs, %d files; matched %d files; total time: %v\n",
			walkedDirs, walkedFiles, skippedDirs, skippedFiles, matchedFiles, time.Since(walkStart))
	}

	return results, nil
}

// gitignoreCache caches compiled gitignore matchers per directory
var (
	gitignoreCache = make(map[string]*gitignore.GitIgnore)
	gitignoreMutex sync.RWMutex
)

// loadGitIgnore loads and compiles .gitignore files for a directory
func loadGitIgnore(dir string) (*gitignore.GitIgnore, error) {
	gitignoreMutex.RLock()
	if gi, ok := gitignoreCache[dir]; ok {
		gitignoreMutex.RUnlock()
		return gi, nil
	}
	gitignoreMutex.RUnlock()

	// Try to load .gitignore from the directory
	gitignorePath := filepath.Join(dir, ".gitignore")
	gi, err := gitignore.CompileIgnoreFile(gitignorePath)
	if err != nil {
		// Create empty matcher if no gitignore files found
		gi = gitignore.CompileIgnoreLines()
	}

	// Cache the result
	gitignoreMutex.Lock()
	gitignoreCache[dir] = gi
	gitignoreMutex.Unlock()

	return gi, nil
}

// isIgnoredPath checks if a path should be ignored based on .gitignore patterns
// Note: This function is kept for backward compatibility but is less efficient
// than using loadGitIgnore once and checking multiple paths
func isIgnoredPath(dir, path string) bool {
	// Get relative path for gitignore matching
	relPath, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	relPath = filepath.ToSlash(relPath)

	// Quick check first
	if isBasicIgnoredPath(relPath) {
		return true
	}

	// Load and check .gitignore
	gi, err := loadGitIgnore(dir)
	if err == nil && gi != nil && gi.MatchesPath(relPath) {
		return true
	}

	return false
}

// isBasicIgnoredPath provides fallback checking for common ignored directories
func isBasicIgnoredPath(path string) bool {
	parts := strings.Split(path, "/")
	commonIgnored := []string{".git", ".hg", ".task", "node_modules", "vendor"}
	for _, part := range parts {
		for _, ignored := range commonIgnored {
			if part == ignored {
				return true
			}
		}
	}
	return false
}

// gitignoreFS wraps an fs.FS to filter out ignored paths during traversal
type gitignoreFS struct {
	fsys             fs.FS
	baseDir          string
	respectGitignore bool
}

func (f *gitignoreFS) Open(name string) (fs.File, error) {
	// Check if path is ignored (only if respectGitignore is true)
	if f.respectGitignore && name != "" {
		fullPath := filepath.Join(f.baseDir, name)
		relPath, err := filepath.Rel(f.baseDir, fullPath)
		if err == nil {
			relPathSlash := filepath.ToSlash(relPath)
			if isBasicIgnoredPath(relPathSlash) {
				return nil, fs.ErrNotExist
			}
			gi, _ := loadGitIgnore(f.baseDir)
			if gi != nil && gi.MatchesPath(relPathSlash) {
				return nil, fs.ErrNotExist
			}
		}
	}
	return f.fsys.Open(name)
}

// ReadDir filters out ignored entries (only if respectGitignore is true)
func (f *gitignoreFS) ReadDir(name string) ([]fs.DirEntry, error) {
	readDirStart := time.Now()
	entries, err := fs.ReadDir(f.fsys, name)
	if err != nil {
		return nil, err
	}

	if globDebug {
		fmt.Fprintf(os.Stderr, "[GITIGNOREFS] ReadDir(%q): read %d entries in %v, respectGitignore=%v\n", name, len(entries), time.Since(readDirStart), f.respectGitignore)
	}

	// If not respecting gitignore, return all entries
	if !f.respectGitignore {
		return entries, nil
	}

	// Load gitignore once for this directory
	giStart := time.Now()
	gi, _ := loadGitIgnore(f.baseDir)
	if globDebug && time.Since(giStart) > 1*time.Millisecond {
		fmt.Fprintf(os.Stderr, "[GITIGNOREFS] Loading gitignore for %q took %v\n", name, time.Since(giStart))
	}

	// Filter out ignored entries
	filterStart := time.Now()
	filtered := make([]fs.DirEntry, 0, len(entries))
	skipped := 0
	for _, entry := range entries {
		// Build entry path
		var entryPath string
		if name != "" {
			entryPath = name + "/" + entry.Name()
		} else {
			entryPath = entry.Name()
		}

		// Quick check first
		if isBasicIgnoredPath(entryPath) {
			skipped++
			continue
		}

		// Check gitignore using pre-loaded matcher
		if gi != nil && gi.MatchesPath(entryPath) {
			skipped++
			continue
		}

		filtered = append(filtered, entry)
	}

	if globDebug {
		fmt.Fprintf(os.Stderr, "[GITIGNOREFS] ReadDir(%q): filtered %d/%d entries (skipped %d) in %v\n", 
			name, len(filtered), len(entries), skipped, time.Since(filterStart))
	}

	return filtered, nil
}

// hasShellFeatures checks if pattern needs shell expansion (env vars, tilde, braces)
func hasShellFeatures(pattern string) bool {
	// Check for environment variable syntax: ${VAR} or $VAR (but not $$)
	if strings.Contains(pattern, "${") {
		return true
	}
	// Check for $VAR (but be careful not to match $$ or $ at end of string)
	for i := 0; i < len(pattern)-1; i++ {
		if pattern[i] == '$' && pattern[i+1] != '$' {
			// Check if it's followed by a valid variable name character
			if (pattern[i+1] >= 'A' && pattern[i+1] <= 'Z') ||
				(pattern[i+1] >= 'a' && pattern[i+1] <= 'z') ||
				(pattern[i+1] >= '0' && pattern[i+1] <= '9') ||
				pattern[i+1] == '_' || pattern[i+1] == '{' {
				return true
			}
		}
	}
	// Check for tilde expansion
	if strings.HasPrefix(pattern, "~") || strings.Contains(pattern, "/~/") {
		return true
	}
	// Check for brace expansion: {a,b} or {a..b}
	if strings.Contains(pattern, "{") && strings.Contains(pattern, "}") {
		return true
	}
	return false
}

func collectKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
