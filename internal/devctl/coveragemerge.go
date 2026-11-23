package devctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const atomicProfileHeader = "mode: atomic\n"

// completeUnlinkedCoverage supplies the zero-count blocks go test
// -coverprofile writes for packages that no test binary links. Binary
// covdata contains only linked packages, so textfmt cannot emit those blocks.
func completeUnlinkedCoverage(ctx context.Context, root, profile string, race bool) error {
	pkgs, err := FloorPackages(ctx, root, race)
	if err != nil {
		return err
	}
	// #nosec G304 -- profile is the operator's explicit coverage profile.
	data, err := os.ReadFile(profile)
	if err != nil {
		return fmt.Errorf("read merged coverage profile: %w", err)
	}
	if !bytes.HasPrefix(data, []byte(atomicProfileHeader)) {
		return errors.New("merged coverage profile must use atomic mode")
	}
	missing, err := MissingPackages(bytes.NewReader(data), pkgs)
	if err != nil || len(missing) == 0 {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, goListBudget)
	defer cancel()
	linked, err := linkedTestPackages(ctx, root, race)
	if err != nil {
		return err
	}
	var missingLinked []string
	for _, pkg := range missing {
		if linked[pkg] {
			missingLinked = append(missingLinked, pkg)
		}
	}
	if len(missingLinked) > 0 {
		return missingCoverageError(missingLinked)
	}

	work, err := os.MkdirTemp(filepath.Dir(profile), ".coverage-*")
	if err != nil {
		return fmt.Errorf("create coverage scratch directory: %w", err)
	}
	defer os.RemoveAll(work) //nolint:errcheck // scratch cleanup does not alter the profile
	blocks, err := unlinkedCoverageBlocks(ctx, root, filepath.Join(work, "zeros.out"), missing, race)
	if err != nil {
		return err
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	staged := filepath.Join(work, "complete.out")
	// #nosec G703 -- staged is inside the private scratch directory for the operator's output profile.
	if err := os.WriteFile(staged, append(data, blocks...), 0o600); err != nil {
		return fmt.Errorf("write completed coverage profile: %w", err)
	}
	if err := os.Rename(staged, profile); err != nil {
		return fmt.Errorf("replace merged coverage profile: %w", err)
	}
	return nil
}

// linkedTestPackages takes dependencies only from generated test mains.
// Ordinary commands in go list's output can have deps which no test links.
func linkedTestPackages(ctx context.Context, root string, race bool) (map[string]bool, error) {
	out, err := goListPackages(ctx, root, race, "-test", "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles,Deps", "./...")
	if err != nil {
		return nil, err
	}
	type testPackage struct {
		ImportPath   string
		Dir          string
		TestGoFiles  []string
		XTestGoFiles []string
		Deps         []string
	}
	linked := make(map[string]bool)
	expectedMains := make(map[string]string)
	var candidates []testPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg testPackage
		err := dec.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode Go test package graph: %w", err)
		}
		if strings.Contains(pkg.ImportPath, " [") {
			continue // Go's test-specific copies are not the package roots.
		}
		if len(pkg.TestGoFiles)+len(pkg.XTestGoFiles) > 0 {
			linked[pkg.ImportPath] = true
			expectedMains[pkg.ImportPath+".test"] = pkg.Dir
		}
		if strings.HasSuffix(pkg.ImportPath, ".test") {
			candidates = append(candidates, pkg)
		}
	}

	// The generated test main shares its owner's directory. A real
	// directory named pkg.test can otherwise have the same import path.
	for _, main := range candidates {
		dir, found := expectedMains[main.ImportPath]
		if !found || dir != main.Dir {
			continue
		}
		delete(expectedMains, main.ImportPath)
		// This variant contains external test sources, never the production
		// package at the coincidentally named pkg_test import path.
		testSources := strings.TrimSuffix(main.ImportPath, ".test") + "_test [" + main.ImportPath + "]"
		for _, dep := range main.Deps {
			if dep != testSources {
				name, _, _ := strings.Cut(dep, " [")
				linked[name] = true
			}
		}
	}
	if len(expectedMains) != 0 {
		return nil, errors.New("test package graph lacks generated test mains")
	}
	return linked, nil
}

func unlinkedCoverageBlocks(ctx context.Context, root, scratch string, missing []string, race bool) ([]byte, error) {
	// These explicit packages have no Go test files. Go builds them and
	// emits coverage blocks with zero counts without executing a test binary.
	args := slices.Concat([]string{"test", "-race=" + strconv.FormatBool(race), "-covermode=atomic", "-coverpkg=" + CoverPattern, "-coverprofile=" + scratch}, missing)
	// #nosec G204 -- operands are fixed switches, our scratch path and Go-listed import paths.
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.WaitDelay = goListWaitDelay
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("generate zero-count coverage: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// #nosec G304 -- scratch is our private Go-generated profile path.
	data, err := os.ReadFile(scratch)
	if err != nil {
		return nil, fmt.Errorf("read zero-count coverage: %w", err)
	}
	blocks, ok := bytes.CutPrefix(data, []byte(atomicProfileHeader))
	if !ok {
		return nil, errors.New("zero-count coverage profile must use atomic mode")
	}
	expected := make(map[string]bool, len(missing))
	for _, pkg := range missing {
		expected[pkg] = true
	}
	seen := make(map[string]bool, len(missing))
	for raw := range strings.Lines(string(blocks)) {
		line := strings.TrimSuffix(raw, "\n")
		pkg, err := coverageBlockPackage(line)
		if err != nil {
			return nil, err
		}
		if !expected[pkg] || !strings.HasSuffix(line, " 0") {
			return nil, fmt.Errorf("unexpected zero-count coverage block for %s", pkg)
		}
		seen[pkg] = true
	}
	for _, pkg := range missing {
		if !seen[pkg] {
			return nil, fmt.Errorf("zero-count coverage missing for %s", pkg)
		}
	}
	return blocks, nil
}
