// Package releaseevidence validates and canonicalizes the immutable release
// evidence document produced by CI.
package releaseevidence

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
)

// GenerateOptions identifies the exact release boundary and the repository-
// owned inputs used to construct immutable release evidence.
type GenerateOptions struct {
	SchemaPath    string
	InventoryPath string
	PlatformsPath string
	ArchiveDir    string
	PackageDir    string
	GateDir       string
	LinuxGatePath string
	MacOSGatePath string
	OutputPath    string
	Version       string
	Commit        string
	Tree          string
	RunID         int64
	RunAttempt    int64
	RunURL        string
}

// Generate assembles, canonicalizes, and cross-checks one candidate evidence
// document. Inventory and platform data come from scripts/release-inventory;
// this package never carries a second executable, plugin, or platform list.
//
//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func Generate(options GenerateOptions) error {
	if options.Version == "" || options.RunID <= 0 || options.RunAttempt <= 0 ||
		len(options.Commit) != 40 || len(options.Tree) != 40 ||
		!strings.HasSuffix(options.RunURL, "/"+strconv.FormatInt(options.RunID, 10)) {
		return errors.New("release evidence generation boundary is incomplete")
	}
	inventory, err := decodeJSONObject(options.InventoryPath)
	if err != nil {
		return fmt.Errorf("decode authoritative package inventory: %w", err)
	}
	if !exactObjectKeys(inventory, "executables", "plugin_payloads", "npm_packages") {
		return errors.New("authoritative package inventory has unexpected fields")
	}
	npmPackages, err := npmPackageInventory(inventory)
	if err != nil {
		return err
	}
	platforms, err := readPlatforms(options.PlatformsPath)
	if err != nil {
		return err
	}
	linux, err := decodeGateEvidence(options.LinuxGatePath)
	if err != nil {
		return fmt.Errorf("decode Linux gate evidence: %w", err)
	}
	macos, err := decodeGateEvidence(options.MacOSGatePath)
	if err != nil {
		return fmt.Errorf("decode macOS gate evidence: %w", err)
	}
	archives := make(map[string]any, len(platforms))
	for _, platform := range platforms {
		filename := "agent-sessions-" + options.Version + "-" + platform + ".tar.gz"
		path := filepath.Join(options.ArchiveDir, filename)
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
			return fmt.Errorf("release archive %s is missing or not a regular file", filename)
		}
		digest, digestErr := fileSHA256(path)
		if digestErr != nil {
			return digestErr
		}
		archives[platform] = map[string]any{
			"platform": platform, "filename": filename, "byte_size": info.Size(),
			"sha256": digest, "source_commit": options.Commit, "inventory_verified": true,
		}
	}
	npmArtifacts, err := collectNPMPackageArtifacts(options.PackageDir, npmPackages, options.Version, options.Commit)
	if err != nil {
		return err
	}
	document := map[string]any{
		"schema_version":  1,
		"release_version": options.Version,
		"intended_tag":    "v" + options.Version,
		"commit_sha":      options.Commit,
		"tree_sha":        options.Tree,
		"artifact": map[string]any{
			"file_name":              "agent-sessions-v" + options.Version + "-release-evidence.json",
			"workflow_artifact_name": "agent-sessions-v" + options.Version + "-release-evidence-" + options.Commit,
			"retention_days":         90,
		},
		"workflow": map[string]any{
			"path": ".github/workflows/ci.yml", "run_id": options.RunID,
			"run_attempt": options.RunAttempt, "run_url": options.RunURL,
		},
		"toolchains": map[string]any{
			"linux": linux["toolchain"], "macos": macos["toolchain"],
		},
		"native_clients": map[string]any{
			"linux": linux["native_clients"], "macos": macos["native_clients"],
		},
		"gates": map[string]any{
			"linux": linux["gates"], "macos": macos["gates"],
		},
		"archives":          archives,
		"npm_packages":      npmArtifacts,
		"package_inventory": inventory,
	}
	body, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(options.OutputPath), 0o700); err != nil {
		return err
	}
	raw, err := os.CreateTemp(filepath.Dir(options.OutputPath), ".release-evidence-input-*")
	if err != nil {
		return err
	}
	rawPath := raw.Name()
	defer func() { _ = os.Remove(rawPath) }()
	if _, err := raw.Write(body); err != nil {
		_ = raw.Close()
		return err
	}
	if err := raw.Close(); err != nil {
		return err
	}
	if err := Canonicalize(options.SchemaPath, rawPath, options.OutputPath); err != nil {
		return err
	}
	return CrossCheck(options.SchemaPath, options.OutputPath, options.ArchiveDir, options.PackageDir, options.GateDir,
		options.Commit, options.Tree, options.RunID)
}

// Canonicalize validates input against the normative schema and writes the
// RFC 8785 JSON Canonicalization Scheme representation followed by one LF.
func Canonicalize(schemaPath, inputPath, outputPath string) error {
	_, body, err := decodeAndValidateSchema(schemaPath, inputPath)
	if err != nil {
		return err
	}
	body, err = canonicalDocument(body)
	if err != nil {
		return err
	}
	return writeAtomic(outputPath, body, 0o600)
}

// CrossCheck verifies the byte-level and cross-field invariants that JSON
// Schema cannot express, including archive hashes and declared inventory.
//
//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func CrossCheck(schemaPath, documentPath, archiveDir, packageDir, gateDir, commit, tree string, runID int64) error {
	value, body, err := decodeAndValidateSchema(schemaPath, documentPath)
	if err != nil {
		return err
	}
	canonical, err := canonicalDocument(body)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, canonical) {
		return errors.New("release evidence is not canonical JSON followed by one LF")
	}
	root := object(value)
	if stringField(root, "commit_sha") != commit || stringField(root, "tree_sha") != tree {
		return errors.New("release evidence commit or tree does not match the checked-out release boundary")
	}
	artifact := object(root["artifact"])
	version := stringField(root, "release_version")
	if version == "" || stringField(root, "intended_tag") != "v"+version ||
		stringField(artifact, "file_name") != "agent-sessions-v"+version+"-release-evidence.json" ||
		stringField(artifact, "workflow_artifact_name") != "agent-sessions-v"+version+"-release-evidence-"+commit {
		return errors.New("release evidence artifact name is not bound to the exact commit")
	}
	workflow := object(root["workflow"])
	if integerField(workflow, "run_id") != runID || !strings.HasSuffix(stringField(workflow, "run_url"), "/"+strconv.FormatInt(runID, 10)) {
		return errors.New("release evidence workflow URL is not bound to its run id")
	}
	for osName, rawGates := range object(root["gates"]) {
		for gateName, rawGate := range object(rawGates) {
			gate := object(rawGate)
			jobURL := stringField(gate, "job_url")
			if !strings.Contains(jobURL, "/actions/runs/"+strconv.FormatInt(runID, 10)+"/job/") {
				return fmt.Errorf("%s %s gate URL is not from workflow run %d", osName, gateName, runID)
			}
			artifact := stringField(gate, "evidence_artifact")
			digest, digestErr := fileSHA256(filepath.Join(gateDir, artifact))
			if digestErr != nil || digest != stringField(gate, "evidence_sha256") {
				return fmt.Errorf("%s %s gate evidence artifact is missing or changed", osName, gateName)
			}
		}
	}
	inventory, err := documentInventory(root["package_inventory"])
	if err != nil {
		return err
	}
	for platform, rawArchive := range object(root["archives"]) {
		archive := object(rawArchive)
		if stringField(archive, "platform") != platform || stringField(archive, "source_commit") != commit {
			return fmt.Errorf("archive %s identity is inconsistent", platform)
		}
		path := filepath.Join(archiveDir, stringField(archive, "filename"))
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != integerField(archive, "byte_size") {
			return fmt.Errorf("archive %s size or file identity does not match evidence", platform)
		}
		digest, digestErr := fileSHA256(path)
		if digestErr != nil || digest != stringField(archive, "sha256") {
			return fmt.Errorf("archive %s SHA-256 does not match evidence", platform)
		}
		if err := verifyArchiveInventory(path, platform, inventory.executables, inventory.pluginPaths); err != nil {
			return fmt.Errorf("archive %s inventory: %w", platform, err)
		}
	}
	if err := verifyNPMPackageArtifacts(packageDir, version, commit, inventory.npmPackages, array(root["npm_packages"])); err != nil {
		return err
	}
	return nil
}

func decodeAndValidateSchema(schemaPath, documentPath string) (any, []byte, error) {
	schema, err := jsonschema.Compile(schemaPath)
	if err != nil {
		return nil, nil, fmt.Errorf("compile release evidence schema: %w", err)
	}
	body, err := os.ReadFile(documentPath) //nolint:gosec // explicit operator/workflow evidence path.
	if err != nil {
		return nil, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, fmt.Errorf("decode release evidence: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, nil, errors.New("release evidence contains trailing JSON values")
		}
		return nil, nil, fmt.Errorf("decode release evidence trailer: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return nil, nil, fmt.Errorf("validate release evidence schema: %w", err)
	}
	return value, body, nil
}

func canonicalDocument(body []byte) ([]byte, error) {
	canonical, err := jsoncanonicalizer.Transform(body)
	if err != nil {
		return nil, fmt.Errorf("canonicalize release evidence as RFC 8785: %w", err)
	}
	return append(canonical, '\n'), nil
}

type npmPackageSpec struct {
	Path string
	Name string
}

type releaseInventory struct {
	executables []string
	pluginPaths []string
	npmPackages []npmPackageSpec
}

func documentInventory(raw any) (releaseInventory, error) {
	inventory := object(raw)
	rawExecutables := array(inventory["executables"])
	executables := make([]string, 0, len(rawExecutables))
	for _, value := range rawExecutables {
		executables = append(executables, value.(string))
	}
	var pluginPaths []string
	for _, rawPlugin := range array(inventory["plugin_payloads"]) {
		for _, rawPath := range array(object(rawPlugin)["archive_paths"]) {
			pluginPaths = append(pluginPaths, rawPath.(string))
		}
	}
	npmPackages, err := npmPackageInventory(inventory)
	if err != nil {
		return releaseInventory{}, err
	}
	if len(executables) == 0 || len(pluginPaths) == 0 {
		return releaseInventory{}, errors.New("release evidence inventory is empty")
	}
	return releaseInventory{executables: executables, pluginPaths: pluginPaths, npmPackages: npmPackages}, nil
}

func npmPackageInventory(inventory map[string]any) ([]npmPackageSpec, error) {
	rawPackages := array(inventory["npm_packages"])
	packages := make([]npmPackageSpec, 0, len(rawPackages))
	seenNames := make(map[string]bool, len(rawPackages))
	seenPaths := make(map[string]bool, len(rawPackages))
	for _, rawPackage := range rawPackages {
		entry := object(rawPackage)
		if !exactObjectKeys(entry, "path", "name") {
			return nil, errors.New("authoritative npm package inventory has unexpected fields")
		}
		packagePath, name := stringField(entry, "path"), stringField(entry, "name")
		if packagePath == "" || name == "" || filepath.IsAbs(packagePath) ||
			filepath.Clean(packagePath) != packagePath || packagePath == "." ||
			strings.HasPrefix(packagePath, ".."+string(filepath.Separator)) ||
			seenNames[name] || seenPaths[packagePath] {
			return nil, errors.New("authoritative npm package inventory is malformed or duplicated")
		}
		seenNames[name], seenPaths[packagePath] = true, true
		packages = append(packages, npmPackageSpec{Path: packagePath, Name: name})
	}
	if len(packages) == 0 {
		return nil, errors.New("authoritative npm package inventory is empty")
	}
	return packages, nil
}

func collectNPMPackageArtifacts(packageDir string, packages []npmPackageSpec, version, commit string) ([]any, error) {
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		return nil, fmt.Errorf("read npm package artifacts: %w", err)
	}
	byName := make(map[string]map[string]any, len(packages))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".tgz" {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
			return nil, fmt.Errorf("npm package artifact %s is not a nonempty regular file", entry.Name())
		}
		path := filepath.Join(packageDir, entry.Name())
		name, packedVersion, manifestErr := readNPMPackageManifest(path)
		if manifestErr != nil {
			return nil, fmt.Errorf("read npm package artifact %s: %w", entry.Name(), manifestErr)
		}
		if _, duplicate := byName[name]; duplicate {
			return nil, fmt.Errorf("npm package artifact for %s is duplicated", name)
		}
		digest, digestErr := fileSHA256(path)
		if digestErr != nil {
			return nil, digestErr
		}
		byName[name] = map[string]any{
			"name": name, "version": packedVersion, "filename": entry.Name(),
			"byte_size": info.Size(), "sha256": digest, "source_commit": commit,
		}
	}
	artifacts := make([]any, 0, len(packages))
	for _, spec := range packages {
		artifact, ok := byName[spec.Name]
		if !ok {
			return nil, fmt.Errorf("npm package artifact for %s is missing", spec.Name)
		}
		if stringField(artifact, "version") != version {
			return nil, fmt.Errorf("npm package artifact %s has version %s, want %s", spec.Name, stringField(artifact, "version"), version)
		}
		artifact["path"] = spec.Path
		artifacts = append(artifacts, artifact)
		delete(byName, spec.Name)
	}
	for name := range byName {
		return nil, fmt.Errorf("npm package artifact %s is not in the authoritative inventory", name)
	}
	return artifacts, nil
}

//nolint:gocyclo // Artifact identity, bytes, manifest, and exact directory membership are one fail-closed boundary.
func verifyNPMPackageArtifacts(packageDir, version, commit string, packages []npmPackageSpec, rawArtifacts []any) error {
	if len(rawArtifacts) != len(packages) {
		return errors.New("npm package artifacts do not match the authoritative inventory")
	}
	wantedFiles := make(map[string]bool, len(packages))
	for index, spec := range packages {
		artifact := object(rawArtifacts[index])
		if stringField(artifact, "path") != spec.Path || stringField(artifact, "name") != spec.Name ||
			stringField(artifact, "version") != version || stringField(artifact, "source_commit") != commit {
			return fmt.Errorf("npm package artifact %s identity is inconsistent", spec.Name)
		}
		filename := stringField(artifact, "filename")
		if filename == "" || filepath.Base(filename) != filename || filepath.Ext(filename) != ".tgz" || wantedFiles[filename] {
			return fmt.Errorf("npm package artifact %s filename is invalid or duplicated", spec.Name)
		}
		wantedFiles[filename] = true
		path := filepath.Join(packageDir, filename)
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != integerField(artifact, "byte_size") {
			return fmt.Errorf("npm package artifact %s size or file identity does not match evidence", spec.Name)
		}
		digest, digestErr := fileSHA256(path)
		if digestErr != nil || digest != stringField(artifact, "sha256") {
			return fmt.Errorf("npm package artifact %s SHA-256 does not match evidence", spec.Name)
		}
		name, packedVersion, manifestErr := readNPMPackageManifest(path)
		if manifestErr != nil || name != spec.Name || packedVersion != version {
			return fmt.Errorf("npm package artifact %s manifest does not match evidence", spec.Name)
		}
	}
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		return fmt.Errorf("read npm package artifacts: %w", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tgz" && !wantedFiles[entry.Name()] {
			return fmt.Errorf("npm package artifact %s is not declared by evidence", entry.Name())
		}
	}
	return nil
}

func readNPMPackageManifest(path string) (string, string, error) {
	file, err := os.Open(path) //nolint:gosec // exact package artifact selected by the release workflow.
	if err != nil {
		return "", "", err
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			return "", "", errors.New("package/package.json is missing")
		}
		if nextErr != nil {
			return "", "", nextErr
		}
		if header.Name != "package/package.json" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 1<<20 {
			return "", "", errors.New("package/package.json is not a bounded regular file")
		}
		body, readErr := io.ReadAll(io.LimitReader(reader, header.Size))
		if readErr != nil {
			return "", "", readErr
		}
		var manifest map[string]any
		if err := json.Unmarshal(body, &manifest); err != nil {
			return "", "", err
		}
		name, version := stringField(manifest, "name"), stringField(manifest, "version")
		if name == "" || version == "" {
			return "", "", errors.New("package manifest has no name or version")
		}
		return name, version, nil
	}
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func verifyArchiveInventory(path, platform string, executables, pluginPaths []string) error {
	file, err := os.Open(path) //nolint:gosec // exact archive named by validated evidence.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	present := map[string]bool{}
	packageRoot := ""
	for {
		header, nextErr := reader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		name := strings.TrimSuffix(header.Name, "/")
		parts := strings.SplitN(name, "/", 2)
		if packageRoot == "" {
			packageRoot = parts[0]
		}
		if parts[0] != packageRoot {
			return errors.New("archive contains more than one package root")
		}
		if len(parts) == 2 {
			present[parts[1]] = true
		}
	}
	for _, executable := range executables {
		if !present["bin/"+platform+"/"+executable] {
			return fmt.Errorf("missing executable %s", executable)
		}
	}
	for _, pluginPath := range pluginPaths {
		found := present[pluginPath]
		if !found {
			prefix := strings.TrimSuffix(pluginPath, "/") + "/"
			for entry := range present {
				if strings.HasPrefix(entry, prefix) {
					found = true
					break
				}
			}
		}
		if !found {
			return fmt.Errorf("missing plugin payload %s", pluginPath)
		}
	}
	return nil
}

func decodeJSONObject(path string) (map[string]any, error) {
	body, err := os.ReadFile(path) //nolint:gosec // explicit repository/workflow input.
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("JSON input contains trailing values")
	}
	return value, nil
}

func decodeGateEvidence(path string) (map[string]any, error) {
	value, err := decodeJSONObject(path)
	if err != nil {
		return nil, err
	}
	if !exactObjectKeys(value, "toolchain", "native_clients", "gates") {
		return nil, errors.New("gate evidence has unexpected fields")
	}
	return value, nil
}

func exactObjectKeys(value map[string]any, names ...string) bool {
	if len(value) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := value[name]; !ok {
			return false
		}
	}
	return true
}

func readPlatforms(path string) ([]string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // exact repository inventory output.
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" || seen[fields[2]] {
			return nil, errors.New("authoritative platform inventory is malformed or duplicated")
		}
		seen[fields[2]] = true
		result = append(result, fields[2])
	}
	if len(result) == 0 {
		return nil, errors.New("authoritative platform inventory is empty")
	}
	return result, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // explicit workflow archive path.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeAtomic(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".release-evidence-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func array(value any) []any {
	result, _ := value.([]any)
	return result
}

func stringField(value map[string]any, name string) string {
	result, _ := value[name].(string)
	return result
}

func integerField(value map[string]any, name string) int64 {
	number, _ := value[name].(json.Number)
	result, _ := number.Int64()
	return result
}
