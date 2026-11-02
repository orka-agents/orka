package protectedset

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

// trustBoundariesADR records the self-improvement enforcement points. Its
// enforcement table is the source the shipped example protected set must
// cover.
const trustBoundariesADR = "docs/adr/0040-self-improvement-trust-boundaries.md"

var (
	backtickedToken = regexp.MustCompile("`([^`]+)`")
	fileLikeToken   = regexp.MustCompile(`\.(go|ya?ml|md|json|sh|tpl)$`)
)

// TestShippedProtectedSetCoversADREnforcementPoints reads every file path
// named in the ADR's "Enforcement point" column and fails when the shipped
// example protected set does not protect it, so the two lists cannot drift.
func TestShippedProtectedSetCoversADREnforcementPoints(t *testing.T) {
	document, err := os.ReadFile(filepath.Join(repositoryRoot, trustBoundariesADR))
	if os.IsNotExist(err) {
		t.Skipf("%s is not in this tree yet", trustBoundariesADR)
	}
	if err != nil {
		t.Fatalf("read ADR: %v", err)
	}
	tokens := enforcementPointFileTokens(t, document)
	if len(tokens) < 5 {
		t.Fatalf("found %d enforcement-point file references in %s, want the full table", len(tokens), trustBoundariesADR)
	}
	_, set := loadSample(t)
	denied := set.DeniedPathsFor(orkaRepository)
	for _, token := range tokens {
		paths := resolveRepositoryPaths(t, token)
		if len(paths) == 0 {
			t.Errorf("enforcement point %q names no file in the repository", token)
		}
		for _, repositoryPath := range paths {
			if !workspacedelta.PathDenied(denied, repositoryPath) {
				t.Errorf("enforcement point %q: %s is missing from %s", token, repositoryPath, sampleConfigMap)
			}
		}
	}
}

// enforcementPointFileTokens returns the backticked file and directory
// references in the "Enforcement point" column of the ADR's rule table. A
// token is a file reference when it has a file extension, or when it names an
// existing repository path; other tokens, such as identifiers and API
// resources like agents/status, are not files.
func enforcementPointFileTokens(t *testing.T, document []byte) []string {
	t.Helper()
	column := -1
	var tokens []string
	scanner := bufio.NewScanner(bytes.NewReader(document))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "|") {
			if column >= 0 {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if column < 0 {
			for index, cell := range cells {
				if strings.TrimSpace(cell) == "Enforcement point" {
					column = index
				}
			}
			continue
		}
		if column >= len(cells) || strings.Trim(strings.TrimSpace(cells[column]), "-:") == "" {
			continue
		}
		for _, match := range backtickedToken.FindAllStringSubmatch(cells[column], -1) {
			token := strings.TrimSpace(match[1])
			if fileLikeToken.MatchString(token) || strings.Contains(token, "/") && repositoryPathExists(token) {
				tokens = append(tokens, token)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan ADR: %v", err)
	}
	if column < 0 {
		t.Fatalf("%s has no table with an \"Enforcement point\" column", trustBoundariesADR)
	}
	return tokens
}

func repositoryPathExists(token string) bool {
	_, err := os.Stat(filepath.Join(repositoryRoot, filepath.FromSlash(strings.TrimSuffix(strings.TrimPrefix(token, "./"), "/"))))
	return err == nil
}

// resolveRepositoryPaths maps an ADR reference to repository-relative file
// paths: a path names itself (or every file below it for a directory), and a
// bare file name names every repository file with that name.
func resolveRepositoryPaths(t *testing.T, token string) []string {
	t.Helper()
	token = strings.TrimSuffix(strings.TrimPrefix(token, "./"), "/")
	if strings.Contains(token, "/") {
		info, err := os.Stat(filepath.Join(repositoryRoot, filepath.FromSlash(token)))
		switch {
		case err != nil:
			// Not built yet: the path itself must already be protected.
			return []string{token}
		case !info.IsDir():
			return []string{token}
		}
		var files []string
		walkRepository(t, token, func(repositoryPath string) { files = append(files, repositoryPath) })
		if len(files) == 0 {
			files = append(files, token+"/placeholder")
		}
		return files
	}
	var files []string
	walkRepository(t, ".", func(repositoryPath string) {
		if path.Base(repositoryPath) == token {
			files = append(files, repositoryPath)
		}
	})
	return files
}

func walkRepository(t *testing.T, start string, visit func(string)) {
	t.Helper()
	root := filepath.Join(repositoryRoot, filepath.FromSlash(start))
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "bin", "vendor", "dist", "testdata":
				return filepath.SkipDir
			}
			if strings.HasPrefix(entry.Name(), ".") && entry.Name() != ".github" && current != root {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(repositoryRoot, current)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository from %s: %v", start, err)
	}
}
