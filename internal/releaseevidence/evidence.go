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

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// GenerateOptions identifies the exact release boundary and the repository-
// owned inputs used to construct immutable release evidence.
type GenerateOptions struct {
	SchemaPath    string
	InventoryPath string
	PlatformsPath string
	ArchiveDir    string
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

type sourceReleaseArchiveManifest struct {
	SchemaVersion      int    `json:"schema_version"`
	ReleaseVersion     string `json:"release_version"`
	HubProtocolVersion int    `json:"hub_protocol_version"`
	Platform           string `json:"platform"`
	Checksums          string `json:"checksums"`
	Executables        []struct {
		Name string `json:"name"`
		Role string `json:"role"`
		Path string `json:"path"`
	} `json:"executables"`
	ConnectorPayloads []struct {
		Product      string   `json:"product"`
		PluginID     string   `json:"plugin_id"`
		ArchivePaths []string `json:"archive_paths"`
	} `json:"connector_payloads"`
	ServiceAssets struct {
		Host []string `json:"host"`
		Hub  []string `json:"hub"`
	} `json:"service_assets"`
}

const (
	maximumReleaseArchiveEntryBytes = 512 * 1024 * 1024
	maximumReleaseArchiveBytes      = 2 * 1024 * 1024 * 1024
	maximumReleaseMetadataBytes     = 16 * 1024 * 1024
)

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
	if !exactObjectKeys(inventory, "executables", "plugin_payloads") {
		return errors.New("authoritative package inventory has unexpected fields")
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
	return CrossCheck(options.SchemaPath, options.OutputPath, options.ArchiveDir, options.GateDir,
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
func CrossCheck(schemaPath, documentPath, archiveDir, gateDir, commit, tree string, runID int64) error {
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
	releaseVersion := stringField(root, "release_version")
	if releaseVersion == "" || stringField(root, "intended_tag") != "v"+releaseVersion {
		return errors.New("release evidence tag is not bound to its declared release version")
	}
	artifact := object(root["artifact"])
	wantEvidenceName := "agent-sessions-v" + releaseVersion + "-release-evidence.json"
	if stringField(artifact, "file_name") != wantEvidenceName ||
		stringField(artifact, "workflow_artifact_name") != strings.TrimSuffix(wantEvidenceName, ".json")+"-"+commit {
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
	executables, pluginPaths, err := documentInventory(root["package_inventory"])
	if err != nil {
		return err
	}
	for platform, rawArchive := range object(root["archives"]) {
		archive := object(rawArchive)
		wantFilename := "agent-sessions-" + releaseVersion + "-" + platform + ".tar.gz"
		if stringField(archive, "platform") != platform || stringField(archive, "source_commit") != commit ||
			stringField(archive, "filename") != wantFilename {
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
		sidecar, sidecarErr := os.ReadFile(path + ".sha256") //nolint:gosec // exact archive checksum companion.
		if sidecarErr != nil || string(sidecar) != digest+"  "+filepath.Base(path)+"\n" {
			return fmt.Errorf("archive %s checksum sidecar is missing or inconsistent", platform)
		}
		if err := verifyArchiveInventory(path, releaseVersion, platform, executables, pluginPaths); err != nil {
			return fmt.Errorf("archive %s inventory: %w", platform, err)
		}
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

func documentInventory(raw any) ([]string, []string, error) {
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
	if len(executables) == 0 || len(pluginPaths) == 0 {
		return nil, nil, errors.New("release evidence inventory is empty")
	}
	return executables, pluginPaths, nil
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func verifyArchiveInventory(path, releaseVersion, platform string, executables, pluginPaths []string) error {
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
	regularDigests := map[string]string{}
	var manifestBody, checksumBody []byte
	packageRoot := ""
	var totalRegularBytes int64
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
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
			if header.Typeflag == tar.TypeReg {
				if header.Size < 0 || header.Size > maximumReleaseArchiveEntryBytes ||
					totalRegularBytes > maximumReleaseArchiveBytes-header.Size {
					return errors.New("release archive regular-file budget exceeded")
				}
				totalRegularBytes += header.Size
				digest := sha256.New()
				var body []byte
				if parts[1] == "manifest.json" || parts[1] == "SHA256SUMS" {
					if header.Size > maximumReleaseMetadataBytes {
						return errors.New("release archive metadata budget exceeded")
					}
					body, nextErr = io.ReadAll(io.LimitReader(io.TeeReader(reader, digest), header.Size+1))
					if nextErr == nil && int64(len(body)) != header.Size {
						nextErr = io.ErrUnexpectedEOF
					}
				} else {
					_, nextErr = io.CopyN(digest, reader, header.Size)
				}
				if nextErr != nil {
					return nextErr
				}
				regularDigests[parts[1]] = hex.EncodeToString(digest.Sum(nil))
				switch parts[1] {
				case "manifest.json":
					manifestBody = body
				case "SHA256SUMS":
					checksumBody = body
				}
			}
		}
	}
	if packageRoot != "agent-sessions-"+releaseVersion+"-"+platform {
		return fmt.Errorf("package root %q does not match release and platform", packageRoot)
	}
	expectedBinaries := make(map[string]bool, len(executables))
	for _, executable := range executables {
		binaryPath := "bin/" + platform + "/" + executable
		expectedBinaries[binaryPath] = true
		if !present[binaryPath] {
			return fmt.Errorf("missing executable %s", executable)
		}
	}
	for entry := range regularDigests {
		if strings.HasPrefix(entry, "bin/") && !expectedBinaries[entry] {
			return fmt.Errorf("unexpected executable image %s", entry)
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
	if err := verifySourceReleaseManifest(manifestBody, releaseVersion, platform, executables, pluginPaths, present); err != nil {
		return err
	}
	if err := verifyInternalChecksums(checksumBody, regularDigests); err != nil {
		return err
	}
	return nil
}

//nolint:gocyclo // Manifest verification keeps every closed inventory dimension visible in one fail-closed boundary.
func verifySourceReleaseManifest(
	body []byte,
	releaseVersion, platform string,
	executables, pluginPaths []string,
	present map[string]bool,
) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest sourceReleaseArchiveManifest
	if len(body) == 0 || decoder.Decode(&manifest) != nil {
		return errors.New("generated source-release manifest is missing or malformed")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("generated source-release manifest is missing or malformed")
	}
	if manifest.SchemaVersion != 1 || manifest.ReleaseVersion != releaseVersion ||
		manifest.HubProtocolVersion != productcatalog.ProtocolVersion ||
		manifest.Platform != platform || manifest.Checksums != "SHA256SUMS" {
		return errors.New("generated source-release manifest identity is inconsistent")
	}
	if len(manifest.Executables) != len(executables) {
		return errors.New("generated source-release manifest executable inventory is incomplete")
	}
	for index, executable := range executables {
		role := "hub"
		if executable == "agent-sessions" {
			role = "host"
		}
		entry := manifest.Executables[index]
		if entry.Name != executable || entry.Role != role || entry.Path != "bin/"+platform+"/"+executable {
			return errors.New("generated source-release manifest executable identity drifted")
		}
	}
	wantPluginPaths := make(map[string]bool, len(pluginPaths))
	for _, path := range pluginPaths {
		wantPluginPaths[path] = true
	}
	gotPluginPaths := map[string]bool{}
	wantProducts := []string{"codex", "claude", "grok", "qwen"}
	if len(manifest.ConnectorPayloads) != len(wantProducts) {
		return errors.New("generated source-release manifest connector inventory is incomplete")
	}
	for index, connector := range manifest.ConnectorPayloads {
		if connector.Product != wantProducts[index] || connector.PluginID != "agent-sessions" || len(connector.ArchivePaths) == 0 {
			return errors.New("generated source-release manifest connector identity drifted")
		}
		for _, path := range connector.ArchivePaths {
			if !wantPluginPaths[path] || gotPluginPaths[path] {
				return errors.New("generated source-release manifest connector paths drifted")
			}
			gotPluginPaths[path] = true
		}
	}
	if len(gotPluginPaths) != len(wantPluginPaths) || len(manifest.ServiceAssets.Host) == 0 || len(manifest.ServiceAssets.Hub) == 0 {
		return errors.New("generated source-release manifest payload inventory is incomplete")
	}
	for _, assets := range [][]string{manifest.ServiceAssets.Host, manifest.ServiceAssets.Hub} {
		for _, asset := range assets {
			if !present[asset] {
				return fmt.Errorf("generated source-release manifest service asset is missing: %s", asset)
			}
		}
	}
	return nil
}

func verifyInternalChecksums(body []byte, regularDigests map[string]string) error {
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return errors.New("archive SHA256SUMS is missing or malformed")
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		digest, path, ok := strings.Cut(line, "  ")
		if !ok || len(digest) != 64 || path == "" || path == "SHA256SUMS" || seen[path] ||
			strings.HasPrefix(path, "/") || strings.Contains(path, "../") || regularDigests[path] != digest {
			return errors.New("archive SHA256SUMS contains an invalid or inconsistent entry")
		}
		seen[path] = true
	}
	if len(seen) != len(regularDigests)-1 {
		return errors.New("archive SHA256SUMS does not cover every staged regular file")
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
