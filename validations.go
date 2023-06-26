package main

import (
	"bufio"
	"bytes"
	"context"
	"debug/elf"
	"debug/gosym"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	mapset "github.com/deckarep/golang-set/v2"
	v1 "github.com/openshift/api/image/v1"
)

var (
	ErrNotGolangExe  = errors.New("not golang executable")
	ErrNotExecutable = errors.New("not a regular executable")
)

type Baton struct {
	TopDir            string
	GoNoCrypto        bool
	GoVersion         string
	GoVersionDetailed []byte
}

type ValidationFn func(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error

var validationFns = map[string][]ValidationFn{
	"go": {
		validateGoVersion,
		validateGoCgo,
		validateGoCGOInit,
		validateGoTags,
		validateGoSymbols,
		validateGoStatic,
		validateGoOpenssl,
	},
	"exe": {
		validateExe,
	},
}

func validateGoVersion(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error {
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "version", "-m", path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return err
	}

	golangVersionRegexp := regexp.MustCompile(`go(\d.\d\d)`)
	matches := golangVersionRegexp.FindAllStringSubmatch(stdout.String(), -1)
	if len(matches) == 0 {
		return fmt.Errorf("go: could not find compiler version in binary")
	}
	baton.GoVersion = matches[0][1]
	baton.GoVersionDetailed = stdout.Bytes()
	return nil
}

func validateGoSymbols(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error {
	symtable, err := readTable(path)
	if err != nil {
		return fmt.Errorf("go: could not read table for %v: %v", filepath.Base(path), err)
	}
	// Skip if the golang binary is not using crypto
	if !isUsingCryptoModule(symtable) {
		baton.GoNoCrypto = true
		return nil
	}

	v, err := semver.NewVersion(baton.GoVersion)
	if err != nil {
		return fmt.Errorf("go: error creating semver version: %w", err)
	}
	c, err := semver.NewConstraint("< 1.18")
	if err != nil {
		return fmt.Errorf("go: error creating semver constraint: %w", err)
	}
	if c.Check(v) {
		return nil
	}

	if err := ExpectedSyms(requiredGolangSymbols, symtable); err != nil {
		return fmt.Errorf("go: expected symbols (%v) not found for %v: %v", requiredGolangSymbols, filepath.Base(path), err)
	}
	return nil
}

func isUsingCryptoModule(symtable *gosym.Table) bool {
	for _, fn := range symtable.Funcs {
		if strings.Contains(fn.Name, "crypto") {
			return true
		}
	}
	return false
}

func validateGoCgo(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error {
	v, err := semver.NewVersion(baton.GoVersion)
	if err != nil {
		return fmt.Errorf("go: error creating semver version: %w", err)
	}
	c, err := semver.NewConstraint("<= 1.17")
	if err != nil {
		return fmt.Errorf("go: error creating semver constraint: %w", err)
	}
	if c.Check(v) {
		return nil
	}

	if !bytes.Contains(baton.GoVersionDetailed, []byte("CGO_ENABLED=1")) {
		return fmt.Errorf("go: binary is not CGO_ENABLED")
	}
	return nil
}

func validateGoTags(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error {
	invalidTagsSet := mapset.NewSet[string]("no_openssl")
	expectedTagsSet := mapset.NewSet[string]("strictfipsruntime")

	v, err := semver.NewVersion(baton.GoVersion)
	if err != nil {
		return fmt.Errorf("go: error creating semver version: %w", err)
	}
	c, err := semver.NewConstraint("<= 1.17")
	if err != nil {
		return fmt.Errorf("go: error creating semver constraint: %w", err)
	}
	if c.Check(v) {
		return nil
	}

	golangTagsRegexp := regexp.MustCompile(`-tags=(.*)\n`)
	matches := golangTagsRegexp.FindAllSubmatch(baton.GoVersionDetailed, -1)
	if matches == nil {
		return nil
	}

	tags := strings.Split(string(matches[0][1]), ",")
	if len(tags) == 0 {
		return nil
	}

	// check for invalid tags
	binaryTags := mapset.NewSet[string](tags...)
	if set := binaryTags.Intersect(invalidTagsSet); set.Cardinality() > 0 {
		return fmt.Errorf("go: binary has invalid tag %v enabled", set.ToSlice())
	}

	// check for required tags
	if set := binaryTags.Intersect(expectedTagsSet); set.Cardinality() == 0 {
		return fmt.Errorf("go: binary does not contain required tag(s) %v", expectedTagsSet.ToSlice())
	}

	return nil
}

func validateGoStatic(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error {
	// if the static golang binary does not contain crypto then skip
	if baton.GoNoCrypto {
		return nil
	}

	// check for static go
	return validateStaticGo(ctx, path)
}

func validateGoOpenssl(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error {
	// if there is no crypto then skip openssl test
	if baton.GoNoCrypto {
		return nil
	}
	// check for openssl strings
	return validateStringsOpenssl(ctx, path, baton)
}

func validateGoCGOInit(ctx context.Context, tag *v1.TagReference, path string, baton *Baton) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	stream := bufio.NewReader(f)

	cgoInitFound := false
	const cap int = 1 * 1024 * 1024
	buf := make([]byte, cap)

	for {
		n, err := stream.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 || err == io.EOF {
			break
		}
		if bytes.Contains(buf, []byte("cgo_init")) {
			cgoInitFound = true
			break
		}
	}

	if !cgoInitFound {
		return fmt.Errorf("x_cgo_init: not found")
	}

	return nil
}

// scan the binary for multiple libcrypto libraries
func validateStringsOpenssl(ctx context.Context, path string, baton *Baton) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	stream := bufio.NewReader(f)

	libcryptoVersion := ""
	cryptoRegexp := regexp.MustCompile(`libcrypto.so(\.?\d+)*`)
	haveMultipleLibcrypto := false

	const cap int = 1 * 1024 * 1024
	buf := make([]byte, cap)

	for {
		n, err := stream.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 || err == io.EOF {
			break
		}

		matches := cryptoRegexp.FindAllSubmatch(buf, -1)
		if len(matches) == 0 {
			continue
		}
		binaryLibcryptoVersion := string(matches[0][0])
		if binaryLibcryptoVersion == "" {
			continue
		}
		if libcryptoVersion != "" && libcryptoVersion != binaryLibcryptoVersion {
			// Have different libcrypto versions in the same binary.
			haveMultipleLibcrypto = true
		}
		libcryptoVersion = string(matches[0][0])
	}

	if libcryptoVersion == "" {
		return fmt.Errorf("openssl: did not find libcrypto library within binary")
	}

	if haveMultipleLibcrypto {
		return errors.New("openssl: found multiple different libcrypto versions")
	}

	// check for openssl library within container image
	opensslInnerPath := filepath.Join(baton.TopDir, "usr", "lib64", libcryptoVersion)
	if _, err := os.Lstat(opensslInnerPath); err != nil {
		return fmt.Errorf("could not find dependent openssl version %v within container image", libcryptoVersion)
	}

	return nil
}

func validateStaticGo(ctx context.Context, path string) error {
	return isDynamicallyLinked(ctx, path)
}

func isDynamicallyLinked(ctx context.Context, path string) error {
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "file", "-s", path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return err
	}
	if !bytes.Contains(stdout.Bytes(), []byte("dynamically linked")) {
		return fmt.Errorf("exe: executable is statically linked")
	}
	return nil
}

func validateExe(ctx context.Context, _ *v1.TagReference, path string, baton *Baton) error {
	return isDynamicallyLinked(ctx, path)
}

func isGoExecutable(ctx context.Context, path string) error {
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", "version", path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return err
	}
	goVersionRegex := regexp.MustCompile(`.*:\s+go.*`)
	if goVersionRegex.Match(stdout.Bytes()) {
		return nil
	}
	return ErrNotGolangExe
}

// isElfExe checks if path is an ELF executable (which most probably means
// it is a Linux binary).
func isElfExe(path string) (bool, error) {
	exe, err := elf.Open(path)
	if err != nil {
		var elfErr *elf.FormatError
		if errors.As(err, &elfErr) || err == io.EOF {
			// Not an ELF.
			return false, nil
		}
		// Error accessing the file.
		return false, err
	}
	defer exe.Close()
	return exe.Type == elf.ET_EXEC, nil
}

func scanBinary(ctx context.Context, component *OpenshiftComponent, tag *v1.TagReference, topDir, innerPath string) *ScanResult {
	allFn := validationFns["all"]
	goFn := validationFns["go"]
	exeFn := validationFns["exe"]

	baton := &Baton{TopDir: topDir}
	res := NewScanResult().SetComponent(component).SetTag(tag).SetPath(innerPath)

	path := filepath.Join(topDir, innerPath)

	// We are only interested in Linux binaries.
	elf, err := isElfExe(path)
	if err != nil {
		return res.SetError(err)
	}
	if !elf {
		return res.Skipped()
	}

	for _, fn := range allFn {
		if err := fn(ctx, tag, path, baton); err != nil {
			return res.SetError(err)
		}
	}

	// is this a go executable
	if isGoExecutable(ctx, path) == nil {
		for _, fn := range goFn {
			if err := fn(ctx, tag, path, baton); err != nil {
				return res.SetError(err)
			}
		}
	} else {
		// is a regular binary
		for _, fn := range exeFn {
			if err := fn(ctx, tag, path, baton); err != nil {
				return res.SetError(err)
			}
		}
	}

	return res.Success()
}
