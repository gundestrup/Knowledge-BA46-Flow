package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/taxonomy"
)

const (
	maxFixtureDocumentBytes = 200000
	taxonomyFileName        = "taxonomy.yaml"
)

var credentialLike = regexp.MustCompile(`(?i)(\bsk-[a-z0-9]{24,}\b|\bgh[pousr]_[a-z0-9]{20,}\b|\bAKIA[0-9A-Z]{16}\b|\bxox[baprs]-[a-z0-9-]{20,}\b)`)

type sourceManifest struct {
	SchemaVersion int            `json:"schema_version"`
	Taxonomy      *sourceRecord  `json:"taxonomy,omitempty"`
	Documents     []sourceRecord `json:"documents"`
}

type sourceRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Case   string `json:"case"`
}

type copiedSource struct {
	Path   string
	SHA256 string
}

type preparedSource struct {
	Path   string
	SHA256 string
	Data   []byte
}

func main() {
	sourceRoot := flag.String("source-root", "", "root of the local knowledge repository")
	manifestPath := flag.String("manifest", "", "JSON allowlist with source hashes")
	destination := flag.String("destination", "", "new isolated corpus directory")
	flag.Parse()
	if *sourceRoot == "" || *manifestPath == "" || *destination == "" {
		fmt.Fprintln(os.Stderr, "usage: prepare-example-llm-eval -source-root DIR -manifest FILE -destination DIR")
		os.Exit(2)
	}
	copied, err := prepare(*sourceRoot, *manifestPath, *destination)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, file := range copied {
		fmt.Printf("%s  %s\n", file.SHA256, file.Path)
	}
}

func prepare(sourceRoot, manifestPath, destination string) ([]copiedSource, error) {
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	sourceRoot, err = filepath.Abs(sourceRoot)
	if err != nil {
		return nil, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return nil, err
	}
	source, err := os.OpenRoot(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("open source knowledge root: %w", err)
	}
	defer source.Close()
	target, err := os.OpenRoot(destination)
	if err != nil {
		return nil, fmt.Errorf("open isolated fixture root: %w", err)
	}
	defer target.Close()

	seen := make(map[string]bool, len(manifest.Documents))
	prepared := make([]preparedSource, 0, len(manifest.Documents))
	for _, record := range manifest.Documents {
		relative, err := paths.CleanRelative(record.Path)
		if err != nil {
			return nil, fmt.Errorf("unsafe source path %q: %w", record.Path, err)
		}
		relativeSlash := filepath.ToSlash(relative)
		if seen[relativeSlash] {
			return nil, fmt.Errorf("duplicate source path in manifest: %s", relativeSlash)
		}
		seen[relativeSlash] = true
		file, err := readSelectedDocument(source, record, relativeSlash)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, file)
	}
	if manifest.Taxonomy != nil {
		file, err := readSelectedTaxonomy(source, *manifest.Taxonomy)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, file)
	}

	copied := make([]copiedSource, 0, len(prepared))
	for _, file := range prepared {
		copiedFile, err := writeSelectedDocument(target, file)
		if err != nil {
			return nil, err
		}
		copied = append(copied, copiedFile)
	}

	policy, err := target.OpenFile(".kb-policy.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create deny-write fixture policy: %w", err)
	}
	_, writeErr := policy.WriteString("{\"mutations\":\"deny\"}\n")
	closeErr := policy.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return copied, nil
}

func readManifest(path string) (sourceManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sourceManifest{}, fmt.Errorf("read LLM fixture manifest: %w", err)
	}
	var manifest sourceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return sourceManifest{}, fmt.Errorf("parse LLM fixture manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 || len(manifest.Documents) == 0 {
		return sourceManifest{}, fmt.Errorf("unsupported or empty LLM fixture manifest")
	}
	return manifest, nil
}

func readSelectedDocument(source *os.Root, record sourceRecord, relative string) (preparedSource, error) {
	ext := strings.ToLower(filepath.Ext(relative))
	if ext != ".md" && ext != ".markdown" {
		return preparedSource{}, fmt.Errorf("fixture source must be Markdown: %s", relative)
	}
	info, err := source.Lstat(relative)
	if err != nil {
		return preparedSource{}, fmt.Errorf("inspect source %s: %w", relative, err)
	}
	if !info.Mode().IsRegular() {
		return preparedSource{}, fmt.Errorf("source is not a regular file: %s", relative)
	}
	file, err := source.Open(relative)
	if err != nil {
		return preparedSource{}, fmt.Errorf("open source %s: %w", relative, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxFixtureDocumentBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return preparedSource{}, fmt.Errorf("read source %s: %w", relative, readErr)
	}
	if closeErr != nil {
		return preparedSource{}, fmt.Errorf("close source %s: %w", relative, closeErr)
	}
	if len(data) > maxFixtureDocumentBytes {
		return preparedSource{}, fmt.Errorf("source exceeds max_document_bytes: %s", relative)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if !strings.EqualFold(hash, record.SHA256) {
		return preparedSource{}, fmt.Errorf("source changed since curation: %s (got %s)", relative, hash)
	}
	fields, _, err := frontmatter.Parse(string(data))
	if err != nil || frontmatter.NoLLMContext(fields) {
		return preparedSource{}, fmt.Errorf("source is private or has invalid frontmatter policy: %s", relative)
	}
	if credentialLike.Match(data) {
		return preparedSource{}, fmt.Errorf("source contains credential-like material; review before LLM evaluation: %s", relative)
	}
	return preparedSource{Path: relative, SHA256: hash, Data: data}, nil
}

func readSelectedTaxonomy(source *os.Root, record sourceRecord) (preparedSource, error) {
	relative, err := paths.CleanRelative(record.Path)
	if err != nil || filepath.ToSlash(relative) != taxonomyFileName {
		return preparedSource{}, fmt.Errorf("taxonomy sidecar path must be %s", taxonomyFileName)
	}
	info, err := source.Lstat(taxonomyFileName)
	if err != nil {
		return preparedSource{}, fmt.Errorf("inspect %s: %w", taxonomyFileName, err)
	}
	if !info.Mode().IsRegular() {
		return preparedSource{}, fmt.Errorf("%s is not a regular file", taxonomyFileName)
	}
	file, err := source.Open(taxonomyFileName)
	if err != nil {
		return preparedSource{}, fmt.Errorf("open %s: %w", taxonomyFileName, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxFixtureDocumentBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return preparedSource{}, fmt.Errorf("read %s: %w", taxonomyFileName, readErr)
	}
	if closeErr != nil {
		return preparedSource{}, fmt.Errorf("close %s: %w", taxonomyFileName, closeErr)
	}
	if len(data) > maxFixtureDocumentBytes {
		return preparedSource{}, fmt.Errorf("%s exceeds max_fixture_bytes", taxonomyFileName)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if !strings.EqualFold(hash, record.SHA256) {
		return preparedSource{}, fmt.Errorf("%s changed since curation (got %s)", taxonomyFileName, hash)
	}
	if _, err := taxonomy.Parse(data); err != nil {
		return preparedSource{}, fmt.Errorf("validate %s: %w", taxonomyFileName, err)
	}
	return preparedSource{Path: taxonomyFileName, SHA256: hash, Data: data}, nil
}

func writeSelectedDocument(target *os.Root, file preparedSource) (copiedSource, error) {
	relative := file.Path
	if err := target.MkdirAll(filepath.ToSlash(filepath.Dir(relative)), 0700); err != nil {
		return copiedSource{}, fmt.Errorf("create fixture directory for %s: %w", relative, err)
	}
	out, err := target.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return copiedSource{}, fmt.Errorf("create fixture file %s: %w", relative, err)
	}
	_, writeErr := out.Write(file.Data)
	closeErr := out.Close()
	if writeErr != nil {
		return copiedSource{}, fmt.Errorf("write fixture %s: %w", relative, writeErr)
	}
	if closeErr != nil {
		return copiedSource{}, fmt.Errorf("close fixture %s: %w", relative, closeErr)
	}
	return copiedSource{Path: relative, SHA256: file.SHA256}, nil
}
