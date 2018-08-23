// Package analysistest provides utilities for testing analyses.
package analysistest

import (
	"fmt"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/internal/checker"
	"golang.org/x/tools/go/packages"
)

// WriteFiles is a helper function that creates a temporary directory
// and populates it with a GOPATH-style project using filemap (which
// maps file names to contents). On success it returns the name of the
// directory and a cleanup function to delete it.
func WriteFiles(filemap map[string]string) (dir string, cleanup func(), err error) {
	gopath, err := ioutil.TempDir("", "analysistest")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(gopath) }

	for name, content := range filemap {
		filename := filepath.Join(gopath, "src", name)
		os.MkdirAll(filepath.Dir(filename), 0777) // ignore error
		if err := ioutil.WriteFile(filename, []byte(content), 0666); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return gopath, cleanup, nil
}

// TestData returns the effective filename of
// the program's "testdata" directory.
// This function may be overridden by projects using
// an alternative build system (such as Blaze) that
// does not run a test in its package directory.
var TestData = func() string {
	testdata, err := filepath.Abs("testdata")
	if err != nil {
		log.Fatal(err)
	}
	return testdata
}

// Testing is an abstraction of a *testing.T.
type Testing interface {
	Errorf(format string, args ...interface{})
}

// Run applies an analysis to each named package.
// It loads each package from the specified GOPATH-style project
// directory using golang.org/x/tools/go/packages, runs the analysis on
// it, and checks that each the analysis generates the findings
// specified by 'want "..."' comments in the package's source files.
//
// You may wish to call this function from within a (*testing.T).Run
// subtest to ensure that errors have adequate contextual description.
func Run(t Testing, dir string, a *analysis.Analysis, pkgnames ...string) {
	for _, pkgname := range pkgnames {
		pkg, err := loadPackage(dir, pkgname)
		if err != nil {
			t.Errorf("loading %s: %v", pkgname, err)
			continue
		}

		unit, err := checker.Analyze(pkg, a)
		if err != nil {
			t.Errorf("analyzing %s: %v", pkgname, err)
			continue
		}

		checkFindings(t, unit)
	}
}

// loadPackage loads the specified package (from source, with
// dependencies) from dir, which is the root of a GOPATH-style project tree.
func loadPackage(dir, pkgpath string) (*packages.Package, error) {
	// packages.Load loads the real standard library, not a minimal
	// fake version, which would be more efficient, especially if we
	// have many small tests that import, say, net/http.
	// However there is no easy way to make go/packages to consume
	// a list of packages we generate and then do the parsing and
	// typechecking, though this feature seems to be a recurring need.
	//
	// It is possible to write a custom driver, but it's fairly
	// involved and requires setting a global (environment) variable.
	//
	// Also, using the "go list" driver will probably not work in google3.
	//
	// TODO: extend go/packages to allow bypassing the driver.

	cfg := &packages.Config{
		Mode:  packages.LoadAllSyntax,
		Dir:   dir,
		Tests: true,
		Env:   append(os.Environ(), "GOPATH="+dir),
	}
	pkgs, err := packages.Load(cfg, pkgpath)
	if err != nil {
		return nil, err
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("pattern %q expanded to %d packages, want 1",
			pkgpath, len(pkgs))
	}

	return pkgs[0], nil
}

// checkFindings inspects an analysis unit on which the analysis has
// already been run, and verifies that all reported findings match those
// specified by 'want "..."' comments in the package's source files,
// which must have been parsed with comments enabled. Surplus findings
// and unmatched expectations are reported as errors to the Testing.
func checkFindings(t Testing, unit *analysis.Unit) {
	// Read expectations out of comments.
	type key struct {
		file string
		line int
	}
	wantErrs := make(map[key]*regexp.Regexp)
	for _, f := range unit.Syntax {
		for _, c := range f.Comments {
			posn := unit.Fset.Position(c.Pos())
			sanitize(&posn)
			text := strings.TrimSpace(c.Text())
			if !strings.HasPrefix(text, "want") {
				continue
			}
			text = strings.TrimSpace(text[len("want"):])
			pattern, err := strconv.Unquote(text)
			if err != nil {
				t.Errorf("%s: in 'want' comment: %v", posn, err)
				continue
			}
			rx, err := regexp.Compile(pattern)
			if err != nil {
				t.Errorf("%s: %v", posn, err)
				continue
			}
			wantErrs[key{posn.Filename, posn.Line}] = rx
		}
	}

	// Check the findings match expectations.
	for _, f := range unit.Findings {
		posn := unit.Fset.Position(f.Pos)
		sanitize(&posn)
		rx, ok := wantErrs[key{posn.Filename, posn.Line}]
		if !ok {
			t.Errorf("%v: unexpected finding: %v", posn, f.Message)
			continue
		}
		delete(wantErrs, key{posn.Filename, posn.Line})
		if !rx.MatchString(f.Message) {
			t.Errorf("%v: finding %q does not match pattern %q", posn, f.Message, rx)
		}
	}
	for key, rx := range wantErrs {
		t.Errorf("%s:%d: expected finding matching %q", key.file, key.line, rx)
	}
}

// sanitize removes the GOPATH portion of the filename,
// typically a gnarly /tmp directory.
func sanitize(posn *token.Position) {
	// TODO: port to windows.
	if strings.HasPrefix(posn.Filename, "/tmp/") {
		if i := strings.Index(posn.Filename, "/src/"); i > 0 {
			posn.Filename = posn.Filename[i+len("/src/"):]
		}
	}
}
