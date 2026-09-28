package fingerprint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zeebo/xxh3"

	"github.com/XXXDelirious/taskgraph/internal/filepathext"
	"github.com/XXXDelirious/taskgraph/taskfile/ast"
)

// ChecksumChecker validates if a task is up to date by calculating its source
// files checksum
type ChecksumChecker struct {
	tempDir string
	dry     bool
}

func NewChecksumChecker(tempDir string, dry bool) *ChecksumChecker {
	return &ChecksumChecker{
		tempDir: tempDir,
		dry:     dry,
	}
}

func (checker *ChecksumChecker) IsUpToDate(t *ast.Task) (bool, error) {
	if len(t.Sources) == 0 {
		return false, nil
	}

	checksumFile := checker.checksumFilePath(t)

	data, _ := os.ReadFile(checksumFile)
	oldHash := strings.TrimSpace(string(data))

	newHash, files, err := checker.checksumFiles(t)
	if err != nil {
		return false, nil
	}

	if !checker.dry && oldHash != newHash {
		_ = os.MkdirAll(filepathext.SmartJoin(checker.tempDir, "checksum"), 0o755)
		if err = os.WriteFile(checksumFile, []byte(newHash+"\n"), 0o644); err != nil {
			return false, err
		}
	}
	// The manifest lets `--explain` report which files changed. It is kept in
	// step with the checksum file, and written if a previous version of Task
	// left a checksum without one.
	if !checker.dry && (oldHash != newHash || !fileExists(checker.manifestFilePath(t))) {
		if err := writeManifest(checker.manifestFilePath(t), files); err != nil {
			return false, err
		}
	}

	if len(t.Generates) > 0 {
		// For each specified 'generates' field, check whether the files actually exist
		for _, g := range t.Generates {
			// Exclusion patterns don't represent output files; skip them.
			if g.Negate {
				continue
			}
			generates, err := glob(t.Dir, g.Glob)
			if os.IsNotExist(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if len(generates) == 0 {
				return false, nil
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
	_ = os.Remove(checker.manifestFilePath(t))
	return os.Remove(checker.checksumFilePath(t))
}

func (*ChecksumChecker) Kind() string {
	return "checksum"
}

func (c *ChecksumChecker) checksum(t *ast.Task) (string, error) {
	hash, _, err := c.checksumFiles(t)
	return hash, err
}

// checksumFiles returns the checksum of all the task's sources, plus the
// checksum of each source file keyed by its path relative to the task's dir.
// The combined checksum is computed exactly as in Task, so existing
// .task/checksum files stay valid.
func (c *ChecksumChecker) checksumFiles(t *ast.Task) (string, map[string]string, error) {
	sources, err := Globs(t.Dir, t.Sources)
	if err != nil {
		return "", nil, err
	}

	h := xxh3.New()
	fileHash := xxh3.New()
	files := make(map[string]string, len(sources))
	buf := make([]byte, 128*1024)
	for _, name := range sources {
		// also sum the filename, so checksum changes for renaming a file
		if _, err := io.CopyBuffer(h, strings.NewReader(filepath.Base(name)), buf); err != nil {
			return "", nil, err
		}
		f, err := os.Open(name)
		if err != nil {
			return "", nil, err
		}
		fileHash.Reset()
		_, err = io.CopyBuffer(io.MultiWriter(h, fileHash), f, buf)
		f.Close()
		if err != nil {
			return "", nil, err
		}
		sum := fileHash.Sum128()
		files[relativeTo(t.Dir, name)] = fmt.Sprintf("%x%x", sum.Hi, sum.Lo)
	}

	hash := h.Sum128()
	return fmt.Sprintf("%x%x", hash.Hi, hash.Lo), files, nil
}

func (checker *ChecksumChecker) checksumFilePath(t *ast.Task) string {
	return filepath.Join(checker.tempDir, "checksum", normalizeFilename(t.Name()))
}

func (checker *ChecksumChecker) manifestFilePath(t *ast.Task) string {
	return filepath.Join(checker.tempDir, "manifest", normalizeFilename(t.Name())+".json")
}

var checksumFilenameRegexp = regexp.MustCompile("[^A-z0-9]")

// replaces invalid characters on filenames with "-"
func normalizeFilename(f string) string {
	return checksumFilenameRegexp.ReplaceAllString(f, "-")
}
