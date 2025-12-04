package fingerprint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zeebo/xxh3"

	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/taskfile/ast"
)

// ChecksumChecker validates if a task is up to date by calculating its source
// files checksum
type ChecksumChecker struct {
	tempDir string
	dry     bool
	logger  *logger.Logger
}

func NewChecksumChecker(tempDir string, dry bool, l *logger.Logger) *ChecksumChecker {
	return &ChecksumChecker{
		tempDir: tempDir,
		dry:     dry,
		logger:  l,
	}
}

func (checker *ChecksumChecker) IsUpToDate(t *ast.Task) (bool, error) {
	if len(t.Sources) == 0 {
		return false, nil
	}

	checksumFile := checker.checksumFilePath(t)

	data, _ := os.ReadFile(checksumFile)
	oldHash := strings.TrimSpace(string(data))

	newHash, err := checker.checksum(t)
	if err != nil {
		return false, nil
	}

	if !checker.dry && oldHash != newHash {
		_ = os.MkdirAll(filepathext.SmartJoin(checker.tempDir, "checksum"), 0o755)
		if err = os.WriteFile(checksumFile, []byte(newHash+"\n"), 0o644); err != nil {
			return false, err
		}
	}

	if len(t.Generates) > 0 {
		// Collect all generates for logging
		allGenerates := make([]string, 0)
		
		// For each specified 'generates' field, check whether the files actually exist
		for _, g := range t.Generates {
			if g.Negate {
				continue
			}
			generates, err := glob(t.Dir, g.Glob, false)
			if os.IsNotExist(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if len(generates) == 0 {
				return false, nil
			}
			allGenerates = append(allGenerates, generates...)
		}
		
		// Log all discovered generates
		if checker.logger != nil && checker.logger.Verbose {
			checker.logger.VerboseOutf(logger.Cyan, "task: discovered %d generated file(s) for task %q\n", len(allGenerates), t.Task)
			for _, generate := range allGenerates {
				checker.logger.VerboseOutf(logger.Cyan, "task:   - %s\n", generate)
			}
		}
	}

	return oldHash == newHash, nil
}

func (checker *ChecksumChecker) Value(t *ast.Task) (any, error) {
	return checker.checksum(t)
}

func (checker *ChecksumChecker) OnError(t *ast.Task) error {
	if len(t.Sources) == 0 {
		return nil
	}
	return os.Remove(checker.checksumFilePath(t))
}

func (*ChecksumChecker) Kind() string {
	return "checksum"
}

func (c *ChecksumChecker) checksum(t *ast.Task) (string, error) {
	sources, err := Globs(t.Dir, t.Sources)
	if err != nil {
		return "", err
	}
	if c.logger != nil && c.logger.Verbose {
		c.logger.VerboseOutf(logger.Cyan, "task: discovered %d source file(s) for task %q\n", len(sources), t.Task)
		for _, source := range sources {
			c.logger.VerboseOutf(logger.Cyan, "task:   - %s\n", source)
		}
	}

	h := xxh3.New()
	buf := make([]byte, 128*1024)
	for _, f := range sources {
		// also sum the filename, so checksum changes for renaming a file
		if _, err := io.CopyBuffer(h, strings.NewReader(filepath.Base(f)), buf); err != nil {
			return "", err
		}
		f, err := os.Open(f)
		if err != nil {
			return "", err
		}
		if _, err = io.CopyBuffer(h, f, buf); err != nil {
			return "", err
		}
		f.Close()
	}

	hash := h.Sum128()
	return fmt.Sprintf("%x%x", hash.Hi, hash.Lo), nil
}

func (checker *ChecksumChecker) checksumFilePath(t *ast.Task) string {
	return filepath.Join(checker.tempDir, "checksum", normalizeFilename(t.Name()))
}

var checksumFilenameRegexp = regexp.MustCompile("[^A-z0-9]")

// replaces invalid characters on filenames with "-"
func normalizeFilename(f string) string {
	return checksumFilenameRegexp.ReplaceAllString(f, "-")
}
