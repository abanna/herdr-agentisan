package devctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// CoverPattern is the package pattern used by the coverage gate.
const CoverPattern = "./internal/..."

// Package is a package selected by the coverage gate.
type Package struct {
	ImportPath    string
	GoFiles       []string // Selected non-test Go sources, including cgo.
	HasStatements bool
}

// FloorPackages lists internal packages in the profile's race configuration
// and identifies those with instrumentable non-test statements.
func FloorPackages(ctx context.Context, root string, race bool) ([]Package, error) {
	ctx, cancel := context.WithTimeout(ctx, goListBudget)
	defer cancel()
	out, err := goListPackages(ctx, root, race, "-json=ImportPath,Dir,GoFiles,CgoFiles", CoverPattern)
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	var packages []Package
	for {
		var listed struct {
			Dir        string
			ImportPath string
			GoFiles    []string
			CgoFiles   []string
		}
		err := dec.Decode(&listed)
		if errors.Is(err, io.EOF) {
			return packages, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode coverage package list: %w", err)
		}
		listed.GoFiles = append(listed.GoFiles, listed.CgoFiles...)
		hasStatements, err := packageHasStatements(listed.Dir, listed.GoFiles)
		if err != nil {
			return nil, err
		}
		packages = append(packages, Package{ImportPath: listed.ImportPath, GoFiles: listed.GoFiles, HasStatements: hasStatements})
	}
}

func packageHasStatements(dir string, files []string) (bool, error) {
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, fmt.Errorf("parse coverage source: %w", err)
		}
		var hasStatements bool
		ast.Inspect(file, func(node ast.Node) bool {
			if hasStatements {
				return false
			}
			var body *ast.BlockStmt
			switch fn := node.(type) {
			case *ast.FuncDecl:
				// Go's cover skips blank functions and their entire body.
				if fn.Name.Name == "_" {
					return false
				}
				body = fn.Body
			case *ast.FuncLit:
				body = fn.Body
			}
			hasStatements = body != nil && len(body.List) > 0
			return !hasStatements
		})
		if hasStatements {
			return true, nil
		}
	}
	return false, nil
}

// MissingPackages names packages with statements that have no block in a
// text coverage profile. The percentage check validates the full profile
// grammar with go tool cover.
func MissingPackages(profile io.Reader, pkgs []Package) ([]string, error) {
	sc := bufio.NewScanner(profile)
	if sc.Scan() && !strings.HasPrefix(sc.Text(), "mode: ") {
		return nil, errors.New("coverage profile has no mode header")
	}
	seen := make(map[string]bool)
	for sc.Scan() {
		line := sc.Text()
		colon := strings.LastIndexByte(line, ':')
		if colon <= 0 {
			return nil, errors.New("coverage profile block has no file and position separator")
		}
		seen[path.Dir(line[:colon])] = true
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read coverage profile: %w", err)
	}
	var missing []string
	for _, pkg := range pkgs {
		if pkg.HasStatements && !seen[pkg.ImportPath] {
			missing = append(missing, pkg.ImportPath)
		}
	}
	return missing, nil
}

// ErrMissingCoveragePackages reports that the profile omitted a package with statements.
var ErrMissingCoveragePackages = errors.New("coverage profile lacks blocks")

func checkCoveragePackages(ctx context.Context, root, profile string, race bool) error {
	pkgs, err := FloorPackages(ctx, root, race)
	if err != nil {
		return err
	}
	// #nosec G304 -- the coverage gate reads the operator's --profile file.
	f, err := os.Open(profile)
	if err != nil {
		return fmt.Errorf("open coverage profile: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable
	missing, err := MissingPackages(f, pkgs)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w for %d package(s):\n  - %s", ErrMissingCoveragePackages, len(missing), strings.Join(missing, "\n  - "))
	}
	return nil
}
