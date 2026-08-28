package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxProductionLegacyConfigBytes = 1 << 20

var productionLegacyConfigKeys = map[string]struct{}{
	"PEER_FEDERATOR_HUB": {}, "PEER_FEDERATOR_HOST": {}, "PEER_FEDERATOR_NAME": {},
	"PEER_FEDERATOR_ENABLE_REMOTE_LANES": {},
	"PEER_FEDERATOR_CODEX_LANE":          {}, "PEER_FEDERATOR_CLAUDE_LANE": {},
	"PEER_FEDERATOR_GROK_LANE": {}, "PEER_FEDERATOR_QWEN_LANE": {},
	"QWEN_PEER_QWEN_BIN": {}, "QWEN_HOME": {}, "QWEN_RUNTIME_DIR": {},
	"CLAUDE_CONFIG_DIR": {}, "CLAUDE_PEER_CLAUDE_CONFIG_DIR": {},
}

//nolint:gocyclo // Exact Linux/Darwin sources and scoped missing/ambiguous debt remain separate decisions.
func productionLegacyAdoptStoppedHostConfiguration(
	ctx context.Context,
	sources []LegacyInventorySource,
	observedAt int64,
	request *LegacyAdoptionRequest,
	accumulator *productionLegacyAdoptionAccumulator,
	state *productionLegacyDeliveryAccumulator,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var config LegacyHostConfiguration
	var err error
	requireExactConfiguration := productionLegacyDormantHostStatePresent(sources)
	requestIdentityIsCatalogOwned := productionLegacyDormantHostIdentityPresent(sources, request.HostID)
	switch {
	case productionLegacySourcePath(sources, "systemd-host-agent-env") != "":
		path := productionLegacySourcePath(sources, "systemd-host-agent-env")
		config, err = productionLegacySystemdHostConfiguration(path, observedAt)
		if os.IsNotExist(err) && !requireExactConfiguration && !productionLegacySourceFilePresent(sources, "systemd-host-agent") {
			return nil
		}
	case productionLegacySourcePath(sources, "launchd-host-agent") != "":
		path := productionLegacySourcePath(sources, "launchd-host-agent")
		config, err = productionLegacyLaunchdHostConfiguration(path, observedAt)
		if os.IsNotExist(err) && !requireExactConfiguration {
			return nil
		}
	default:
		if requireExactConfiguration {
			productionLegacyAppendConfigurationDebt(request, state, observedAt, "legacy_host_configuration_missing")
		}
		return nil
	}
	if err != nil {
		cause := "legacy_host_configuration_unreadable"
		if os.IsNotExist(err) {
			cause = "legacy_host_configuration_missing"
		}
		productionLegacyAppendConfigurationDebt(request, state, observedAt, cause)
		return nil
	}
	if !productionLegacyHostConfigurationComplete(config) {
		productionLegacyAppendConfigurationDebt(request, state, observedAt, "legacy_host_configuration_ambiguous")
		return nil
	}
	if config.HostID != request.HostID && requestIdentityIsCatalogOwned {
		productionLegacyAppendConfigurationDebt(request, state, observedAt, "legacy_host_configuration_identity_conflict")
		return nil
	}
	if request.Configuration != nil && request.Configuration.SourceRevision != config.SourceRevision {
		productionLegacyAppendConfigurationDebt(request, state, observedAt, "multiple_legacy_host_configurations")
		return nil
	}
	request.HostID = config.HostID
	request.Configuration = cloneLegacyHostConfiguration(&config)
	request.Hub.HostID, request.Hub.HostName = config.HostID, config.HostName
	request.Hub.HubAddress = config.HubAddress
	if request.Hub.State == "" || request.Hub.State == "disabled" {
		request.Hub.State = "reconnecting"
	}
	accumulator.revisions = append(accumulator.revisions, config.SourceRevision)
	return nil
}

func productionLegacyHostConfigurationComplete(configuration LegacyHostConfiguration) bool {
	return configuration.HostID != "" && configuration.HostName != "" && configuration.HubAddress != "" &&
		validateDaemonHubAddress(configuration.HubAddress) == nil
}

// productionLegacyDormantHostStatePresent distinguishes a true no-agent
// install from a stopped/manual host agent whose exact federation identity
// cannot be reconstructed without its closed-list configuration source.
func productionLegacyDormantHostStatePresent(sources []LegacyInventorySource) bool {
	root := productionLegacySourcePath(sources, "host-agent-state")
	if root == "" {
		return false
	}
	entries, err := readProductionLegacyDirectory(root)
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return true
		}
	}
	return false
}

func productionLegacyDormantHostIdentityPresent(sources []LegacyInventorySource, hostID string) bool {
	root := productionLegacySourcePath(sources, "host-agent-state")
	if root == "" || !durableRecordID.MatchString(hostID) {
		return false
	}
	_, err := readProductionLegacyDirectory(filepath.Join(root, hostID))
	return err == nil || !os.IsNotExist(err)
}

func productionLegacySystemdHostConfiguration(path string, observedAt int64) (LegacyHostConfiguration, error) {
	body, _, err := readProductionLegacyRegular(path, 1, maxProductionLegacyConfigBytes)
	if err != nil {
		return LegacyHostConfiguration{}, err
	}
	values := make(map[string]string)
	for lineNumber, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found || key == "" {
			return LegacyHostConfiguration{}, fmt.Errorf("legacy host environment line %d is malformed", lineNumber+1)
		}
		if _, known := productionLegacyConfigKeys[key]; !known {
			continue // Unknown values may be secrets; they are neither interpreted nor retained.
		}
		if _, duplicate := values[key]; duplicate {
			return LegacyHostConfiguration{}, fmt.Errorf("legacy host environment repeats %s", key)
		}
		unquoted, quoteErr := productionLegacyUnquoteEnvironment(value)
		if quoteErr != nil {
			return LegacyHostConfiguration{}, quoteErr
		}
		values[key] = unquoted
	}
	return productionLegacyHostConfiguration(values, "systemd_agent_env", observedAt)
}

//nolint:gocyclo // The bounded plist shape and each known configuration source require exact validation.
func productionLegacyLaunchdHostConfiguration(path string, observedAt int64) (LegacyHostConfiguration, error) {
	body, _, err := readProductionLegacyRegular(path, 1, maxProductionLegacyConfigBytes)
	if err != nil {
		return LegacyHostConfiguration{}, err
	}
	root, err := decodeProductionLegacyPlist(body)
	if err != nil || root.kind != "dict" {
		return LegacyHostConfiguration{}, errors.New("legacy launchd agent plist is malformed")
	}
	label := root.dictionary["Label"]
	if label.kind != "string" || label.text != "net.antst.peer-federator.agent" {
		return LegacyHostConfiguration{}, errors.New("legacy launchd agent label is incompatible")
	}
	arguments := root.dictionary["ProgramArguments"]
	if arguments.kind != "array" || len(arguments.array) < 2 ||
		arguments.array[0].kind != "string" || !filepath.IsAbs(arguments.array[0].text) ||
		arguments.array[1].kind != "string" || arguments.array[1].text != "agent" {
		return LegacyHostConfiguration{}, errors.New("legacy launchd agent arguments are incomplete")
	}
	values := make(map[string]string)
	if environment := root.dictionary["EnvironmentVariables"]; environment.kind != "" {
		if environment.kind != "dict" {
			return LegacyHostConfiguration{}, errors.New("legacy launchd environment is not a dictionary")
		}
		for key, value := range environment.dictionary {
			if _, known := productionLegacyConfigKeys[key]; !known {
				continue
			}
			if value.kind != "string" {
				return LegacyHostConfiguration{}, fmt.Errorf("legacy launchd environment %s is not a string", key)
			}
			values[key] = value.text
		}
	}
	if err := productionLegacyApplyAgentArguments(arguments.array[2:], values); err != nil {
		return LegacyHostConfiguration{}, err
	}
	configuration, err := productionLegacyHostConfiguration(values, "launchd_agent_plist", observedAt)
	if err != nil {
		return LegacyHostConfiguration{}, err
	}
	return configuration, nil
}

//nolint:gocyclo // Every closed-list configuration field has its own canonicality and bound checks.
func productionLegacyHostConfiguration(
	values map[string]string,
	sourceKind string,
	observedAt int64,
) (LegacyHostConfiguration, error) {
	for key, value := range values {
		if len(value) > 4096 {
			return LegacyHostConfiguration{}, fmt.Errorf("legacy host configuration value %s exceeds its metadata bound", key)
		}
	}
	hostID := strings.TrimSpace(values["PEER_FEDERATOR_HOST"])
	if !durableRecordID.MatchString(hostID) {
		return LegacyHostConfiguration{}, errors.New("legacy host configuration lacks an exact host identity")
	}
	hostName := strings.TrimSpace(values["PEER_FEDERATOR_NAME"])
	if hostName == "" {
		return LegacyHostConfiguration{}, errors.New("legacy host configuration lacks an exact host name")
	}
	hubAddress := strings.TrimSpace(values["PEER_FEDERATOR_HUB"])
	if hubAddress == "" {
		return LegacyHostConfiguration{}, errors.New("legacy host configuration lacks an exact hub address")
	}
	remote, err := productionLegacyBoolean(values["PEER_FEDERATOR_ENABLE_REMOTE_LANES"])
	if err != nil {
		return LegacyHostConfiguration{}, err
	}
	configuration := LegacyHostConfiguration{
		HostID: hostID, HostName: hostName, HubAddress: hubAddress,
		RemoteLanesEnabled: remote, LaneExecutables: make(map[string]string),
		ProductOverrides: make(map[string]ProductOverride), ProfileSelections: make(map[string]string),
		SourceKind: sourceKind, UpdatedAt: observedAt,
	}
	for product, key := range map[string]string{
		"codex": "PEER_FEDERATOR_CODEX_LANE", "claude": "PEER_FEDERATOR_CLAUDE_LANE",
		"grok": "PEER_FEDERATOR_GROK_LANE", "qwen": "PEER_FEDERATOR_QWEN_LANE",
	} {
		if value := strings.TrimSpace(values[key]); value != "" {
			if filepath.Base(value) != value && !migrationAbsoluteCleanPath(value) {
				return LegacyHostConfiguration{}, fmt.Errorf("legacy %s lane executable is not canonical", product)
			}
			configuration.LaneExecutables[product] = value
		}
	}
	qwen := ProductOverride{}
	if value := strings.TrimSpace(values["QWEN_PEER_QWEN_BIN"]); value != "" {
		if filepath.Base(value) != value && !migrationAbsoluteCleanPath(value) {
			return LegacyHostConfiguration{}, errors.New("legacy Qwen executable is not canonical")
		}
		qwen.Executable = value
	}
	if value := strings.TrimSpace(values["QWEN_HOME"]); value != "" {
		if !migrationAbsoluteCleanPath(value) {
			return LegacyHostConfiguration{}, errors.New("legacy Qwen profile is not canonical")
		}
		qwen.Profile = value
		configuration.ProfileSelections["qwen_home"] = value
	}
	if value := strings.TrimSpace(values["QWEN_RUNTIME_DIR"]); value != "" {
		if !migrationAbsoluteCleanPath(value) {
			return LegacyHostConfiguration{}, errors.New("legacy Qwen runtime profile is not canonical")
		}
		configuration.ProfileSelections["qwen_runtime_dir"] = value
	}
	if qwen != (ProductOverride{}) {
		configuration.ProductOverrides["qwen"] = qwen
	}
	claudeConfig := strings.TrimSpace(values["CLAUDE_PEER_CLAUDE_CONFIG_DIR"])
	if claudeConfig == "" {
		claudeConfig = strings.TrimSpace(values["CLAUDE_CONFIG_DIR"])
	}
	if claudeConfig != "" {
		if !migrationAbsoluteCleanPath(claudeConfig) {
			return LegacyHostConfiguration{}, errors.New("legacy Claude profile is not canonical")
		}
		configuration.ProductOverrides["claude"] = ProductOverride{Profile: claudeConfig}
		configuration.ProfileSelections["claude_config_dir"] = claudeConfig
	}
	if err := validateDaemonHubAddress(configuration.HubAddress); err != nil {
		return LegacyHostConfiguration{}, err
	}
	metadata := configuration
	metadata.SourceRevision = ""
	metadata.UpdatedAt = 0
	body, err := json.Marshal(metadata)
	if err != nil {
		return LegacyHostConfiguration{}, err
	}
	digest := sha256.Sum256(body)
	configuration.SourceRevision = "sha256:" + hex.EncodeToString(digest[:])
	return configuration, nil
}

func productionLegacyApplyAgentArguments(arguments []productionLegacyPlistValue, values map[string]string) error {
	flagKeys := map[string]string{
		"--hub": "PEER_FEDERATOR_HUB", "--host": "PEER_FEDERATOR_HOST", "--name": "PEER_FEDERATOR_NAME",
		"--codex-lane": "PEER_FEDERATOR_CODEX_LANE", "--claude-lane": "PEER_FEDERATOR_CLAUDE_LANE",
		"--grok-lane": "PEER_FEDERATOR_GROK_LANE", "--qwen-lane": "PEER_FEDERATOR_QWEN_LANE",
		"--qwen-bin": "QWEN_PEER_QWEN_BIN", "--claude-config-dir": "CLAUDE_PEER_CLAUDE_CONFIG_DIR",
	}
	seen := make(map[string]struct{})
	for index := 0; index < len(arguments); index++ {
		if arguments[index].kind != "string" {
			return errors.New("legacy launchd agent argument is not a string")
		}
		flag := arguments[index].text
		if flag == "--enable-remote-lanes" {
			if _, duplicate := seen[flag]; duplicate {
				return errors.New("legacy launchd agent repeats --enable-remote-lanes")
			}
			seen[flag] = struct{}{}
			if current, exists := values["PEER_FEDERATOR_ENABLE_REMOTE_LANES"]; exists {
				enabled, err := productionLegacyBoolean(current)
				if err != nil || !enabled {
					return errors.New("legacy launchd remote-lane sources conflict")
				}
			}
			values["PEER_FEDERATOR_ENABLE_REMOTE_LANES"] = "true"
			continue
		}
		key, known := flagKeys[flag]
		if !known || index+1 >= len(arguments) || arguments[index+1].kind != "string" {
			return fmt.Errorf("legacy launchd agent has unsupported or incomplete argument %q", flag)
		}
		if _, duplicate := seen[flag]; duplicate {
			return fmt.Errorf("legacy launchd agent repeats argument %q", flag)
		}
		seen[flag] = struct{}{}
		index++
		value := arguments[index].text
		if current, exists := values[key]; exists && current != value {
			return fmt.Errorf("legacy launchd sources conflict for %s", key)
		}
		values[key] = value
	}
	return nil
}

func productionLegacyUnquoteEnvironment(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] != '\'' && value[0] != '"' {
		if strings.ContainsAny(value, "\r\n") {
			return "", errors.New("legacy host environment contains a newline")
		}
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != value[0] {
		return "", errors.New("legacy host environment has an unterminated quote")
	}
	return value[1 : len(value)-1], nil
}

func productionLegacyBoolean(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, errors.New("legacy remote-lane setting is not a boolean")
	}
}

func productionLegacyAppendConfigurationDebt(
	request *LegacyAdoptionRequest,
	state *productionLegacyDeliveryAccumulator,
	observedAt int64,
	cause string,
) {
	productionLegacyAppendDeliveryDebt(request, state, DebtRecord{
		RecordHeader: productionLegacyMigrationHeader(observedAt),
		DebtID:       quiescenceRecordID("legacy-host-config-debt", request.HostID, cause),
		Operation:    "migration_reconcile", ResourceKind: "legacy_host_configuration",
		ResourceIdentity: request.HostID, CauseCode: cause,
		RetryPredicate:  "repair or explicitly select the exact stopped Agent Sessions host configuration",
		ProhibitedScope: "do not read credentials, vendor profiles, transcripts, settings, or unrelated environment values",
	})
}

func productionLegacySourceFilePresent(sources []LegacyInventorySource, id string) bool {
	path := productionLegacySourcePath(sources, id)
	if path == "" {
		return false
	}
	_, _, err := readProductionLegacyRegular(path, 1, maxProductionLegacyConfigBytes)
	return err == nil
}

type productionLegacyPlistValue struct {
	kind       string
	text       string
	array      []productionLegacyPlistValue
	dictionary map[string]productionLegacyPlistValue
}

func decodeProductionLegacyPlist(body []byte) (productionLegacyPlistValue, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err != nil {
			return productionLegacyPlistValue{}, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "plist" {
			continue
		}
		for {
			token, err = decoder.Token()
			if err != nil {
				return productionLegacyPlistValue{}, err
			}
			if valueStart, ok := token.(xml.StartElement); ok {
				return decodeProductionLegacyPlistValue(decoder, valueStart)
			}
		}
	}
}

//nolint:gocyclo // This intentionally small XML decoder rejects every unsupported plist node explicitly.
func decodeProductionLegacyPlistValue(
	decoder *xml.Decoder,
	start xml.StartElement,
) (productionLegacyPlistValue, error) {
	switch start.Name.Local {
	case "string", "key":
		var text string
		if err := decoder.DecodeElement(&text, &start); err != nil {
			return productionLegacyPlistValue{}, err
		}
		return productionLegacyPlistValue{kind: start.Name.Local, text: text}, nil
	case "true", "false":
		if err := decoder.Skip(); err != nil {
			return productionLegacyPlistValue{}, err
		}
		return productionLegacyPlistValue{kind: "bool", text: start.Name.Local}, nil
	case "array":
		var result []productionLegacyPlistValue
		for {
			token, err := decoder.Token()
			if err != nil {
				return productionLegacyPlistValue{}, err
			}
			switch typed := token.(type) {
			case xml.StartElement:
				value, err := decodeProductionLegacyPlistValue(decoder, typed)
				if err != nil {
					return productionLegacyPlistValue{}, err
				}
				result = append(result, value)
			case xml.EndElement:
				if typed.Name.Local == "array" {
					return productionLegacyPlistValue{kind: "array", array: result}, nil
				}
			}
		}
	case "dict":
		result := make(map[string]productionLegacyPlistValue)
		pendingKey := ""
		for {
			token, err := decoder.Token()
			if err != nil {
				return productionLegacyPlistValue{}, err
			}
			switch typed := token.(type) {
			case xml.StartElement:
				value, err := decodeProductionLegacyPlistValue(decoder, typed)
				if err != nil {
					return productionLegacyPlistValue{}, err
				}
				if value.kind == "key" {
					if pendingKey != "" || value.text == "" {
						return productionLegacyPlistValue{}, errors.New("legacy plist dictionary key is invalid")
					}
					pendingKey = value.text
					continue
				}
				if pendingKey == "" {
					return productionLegacyPlistValue{}, errors.New("legacy plist dictionary value lacks a key")
				}
				if _, duplicate := result[pendingKey]; duplicate {
					return productionLegacyPlistValue{}, fmt.Errorf("legacy plist repeats key %q", pendingKey)
				}
				result[pendingKey], pendingKey = value, ""
			case xml.EndElement:
				if typed.Name.Local == "dict" {
					if pendingKey != "" {
						return productionLegacyPlistValue{}, errors.New("legacy plist dictionary key lacks a value")
					}
					return productionLegacyPlistValue{kind: "dict", dictionary: result}, nil
				}
			}
		}
	default:
		return productionLegacyPlistValue{}, fmt.Errorf("legacy plist contains unsupported value %q", start.Name.Local)
	}
}
