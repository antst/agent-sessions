package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

const maxLaneCommandInputBytes = 1024 * 1024

// LaneCommandRequest is the canonical local CLI/model-facing lane boundary.
// Arguments contain only Agent Sessions-owned options after Command; Input is
// bounded prompt content already read by the short-lived caller.
type LaneCommandRequest struct {
	Product   string   `json:"product"`
	Command   string   `json:"command"`
	Host      string   `json:"host,omitempty"`
	Arguments []string `json:"arguments,omitempty"`
	Input     string   `json:"input,omitempty"`
}

// LaneCommandNormalizationRequest gives the product adapter the already
// resolved daemon-owned common context plus the exact canonical argv. Product
// adapters reuse their existing parsers here; no native process is started.
type LaneCommandNormalizationRequest struct {
	Product        string
	Command        string
	Arguments      []string
	LaneSessionID  string
	Name           string
	Cwd            string
	PermissionMode string
	NativeActor    map[string]any
}

// LaneCommandNormalization contains non-secret effective native options. A
// rollback is used only for pre-accept resources such as a detached worktree.
type LaneCommandNormalization struct {
	Cwd            string
	PermissionMode string
	NativeOptions  map[string]any
	Rollback       func() error
}

// LaneCommandNormalizer preserves each product's existing option contract at
// the daemon admission boundary.
type LaneCommandNormalizer interface {
	// NormalizeLaneCommand validates product options without starting native work.
	NormalizeLaneCommand(context.Context, LaneCommandNormalizationRequest) (LaneCommandNormalization, error)
}

type laneCommandOptions struct {
	name, cwd, timeout, promptFile, notify string
	groups                                 []string
	inheritGroups, noInheritGroups         bool
	persistent, noAutoArchive, noNotify    bool
	autoArchiveAfter                       string
	allowDuplicateName                     bool
	all, mine, json                        bool
	permissionMode                         string
	productArguments                       []string
}

//nolint:gocyclo // Canonical lane commands intentionally share one authorization and response boundary.
func executeLaneCommand(
	ctx context.Context,
	engine *LaneEngine,
	attachments *AttachmentRegistry,
	sourceAttachmentID string,
	request LaneCommandRequest,
) (map[string]any, error) {
	if engine == nil || attachments == nil {
		return nil, errors.New("lane authority is not ready")
	}
	parent, ok := attachments.attachedByID(sourceAttachmentID)
	if !ok {
		return nil, ErrAttachmentNotAttested
	}
	request.Product = strings.TrimSpace(request.Product)
	if !supportedLaneCommandProduct(request.Product) {
		return nil, fmt.Errorf("unsupported lane product %q", request.Product)
	}
	if len(request.Input) > maxLaneCommandInputBytes {
		return nil, fmt.Errorf("lane input exceeds %d bytes", maxLaneCommandInputBytes)
	}
	options, positionals, err := parseLaneCommandOptions(request.Product, request.Command, request.Arguments)
	if err != nil {
		return nil, err
	}
	command := strings.TrimSpace(request.Command)
	base := map[string]any{"type": "lane." + command, "product": request.Product, "command": command}

	switch command {
	case "run", "start":
		if len(positionals) != 0 {
			return nil, fmt.Errorf("lane %s does not accept a target", command)
		}
		if strings.TrimSpace(options.name) == "" {
			return nil, fmt.Errorf("lane %s requires --name", command)
		}
		laneID, err := randomControlRequestID()
		if err != nil {
			return nil, err
		}
		return startLaneCommand(ctx, engine, parent, sourceAttachmentID, request, options, laneID, command == "run", base)
	case "resume":
		if len(positionals) != 1 {
			return nil, errors.New("lane resume requires one session id or unique visible name")
		}
		lane, err := resolveLaneCommandSelector(ctx, engine, sourceAttachmentID, request.Product, positionals[0], true, false)
		if err != nil {
			return nil, err
		}
		if options.name != "" && options.name != lane.Name {
			return nil, errors.New("lane resume cannot change the durable lane name")
		}
		if options.cwd != "" && resolveLaneCommandCwd(parent.Cwd, options.cwd) != lane.Cwd {
			return nil, errors.New("lane resume cannot change the durable lane cwd")
		}
		return startLaneCommand(ctx, engine, parent, sourceAttachmentID, request, options, lane.LaneSessionID, true, base)
	case "wait":
		if len(positionals) != 1 {
			return nil, errors.New("lane wait requires one session id or unique visible name")
		}
		lane, err := resolveLaneCommandSelector(ctx, engine, sourceAttachmentID, request.Product, positionals[0], true, false)
		if err != nil {
			return nil, err
		}
		turn, err := engine.latestLaneTurn(ctx, lane.LaneSessionID, sourceAttachmentID)
		if err != nil {
			return nil, err
		}
		waitCtx, cancel, err := laneCommandWaitContext(ctx, options.timeout)
		if err != nil {
			return nil, err
		}
		defer cancel()
		collection, err := engine.Wait(waitCtx, LaneCollectRequest{
			LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, SourceAttachmentID: sourceAttachmentID,
		})
		if err != nil {
			return nil, err
		}
		base["lane"], base["turn"], base["collection"] = publicLaneRecord(collection.Lane), publicLaneTurn(collection.Turn), publicLaneCollection(collection)
		return base, nil
	case "status":
		if len(positionals) != 1 {
			return nil, errors.New("lane status requires one session id or unique visible name")
		}
		lane, err := resolveLaneCommandSelector(ctx, engine, sourceAttachmentID, request.Product, positionals[0], true, options.mine)
		if err != nil {
			return nil, err
		}
		base["lane"] = publicLaneRecord(lane)
		if turn, turnErr := engine.latestLaneTurn(ctx, lane.LaneSessionID, sourceAttachmentID); turnErr == nil {
			base["turn"] = publicLaneTurn(turn)
		}
		return base, nil
	case "interrupt":
		if len(positionals) != 1 {
			return nil, errors.New("lane interrupt requires one session id or unique visible name")
		}
		lane, err := resolveLaneCommandSelector(ctx, engine, sourceAttachmentID, request.Product, positionals[0], false, options.mine)
		if err != nil {
			return nil, err
		}
		if lane.ActiveTurnID == "" {
			return nil, ErrLaneNotTerminal
		}
		turn, err := engine.Interrupt(ctx, LaneCollectRequest{
			LaneSessionID: lane.LaneSessionID, TurnID: lane.ActiveTurnID, SourceAttachmentID: sourceAttachmentID,
		})
		if err != nil {
			return nil, err
		}
		base["lane"] = publicLaneRecord(lane)
		base["turn"] = publicLaneTurn(turn)
		return base, nil
	case "archive":
		if len(positionals) != 1 {
			return nil, errors.New("lane archive requires one session id or unique visible name")
		}
		lane, err := resolveLaneCommandSelector(ctx, engine, sourceAttachmentID, request.Product, positionals[0], true, options.mine)
		if err != nil {
			return nil, err
		}
		lane, err = engine.Archive(ctx, LaneArchiveRequest{LaneSessionID: lane.LaneSessionID, SourceAttachmentID: sourceAttachmentID})
		if err != nil {
			return nil, err
		}
		base["lane"] = publicLaneRecord(lane)
		return base, nil
	case "list", "doctor":
		if len(positionals) != 0 {
			return nil, fmt.Errorf("lane %s does not accept a target", command)
		}
		lanes, err := engine.List(ctx, LaneListRequest{SourceAttachmentID: sourceAttachmentID, All: options.all, Mine: options.mine})
		if err != nil {
			return nil, err
		}
		visible := make([]LaneRecord, 0, len(lanes))
		for _, lane := range lanes {
			if lane.Product == request.Product {
				visible = append(visible, publicLaneRecord(lane))
			}
		}
		base["lanes"] = visible
		if command == "doctor" {
			base["ready"], base["authority"] = true, "daemon"
		}
		return base, nil
	default:
		return nil, fmt.Errorf("unsupported lane lifecycle command %q", command)
	}
}

//nolint:gocyclo // Normalization, durable acceptance, rollback, and optional wait form one command transaction.
func startLaneCommand(
	ctx context.Context,
	engine *LaneEngine,
	parent AttachmentRecord,
	sourceAttachmentID string,
	request LaneCommandRequest,
	options laneCommandOptions,
	laneID string,
	wait bool,
	base map[string]any,
) (map[string]any, error) {
	if strings.TrimSpace(request.Input) == "" {
		return nil, errors.New("lane prompt is empty")
	}
	if existing, _ := engine.List(ctx, LaneListRequest{SourceAttachmentID: sourceAttachmentID, All: false}); !options.allowDuplicateName {
		name := strings.TrimSpace(options.name)
		for _, lane := range existing {
			if lane.Product == request.Product && name != "" && lane.Name == name && lane.LaneSessionID != laneID {
				return nil, ErrLaneIdempotencyConflict
			}
		}
	}
	turnID, err := randomControlRequestID()
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(options.name)
	cwd := resolveLaneCommandCwd(parent.Cwd, options.cwd)
	var existingActor map[string]any
	if existing, readErr := engine.ReadLane(ctx, laneID); readErr == nil {
		name, cwd = existing.Name, existing.Cwd
		existingActor = cloneAttachmentEvidence(existing.NativeActor)
	}
	permission := laneCommandPermission(request.Product, parent.PermissionMode, options.permissionMode)
	normalized, err := engine.normalizeLaneCommand(ctx, LaneCommandNormalizationRequest{
		Product: request.Product, Command: request.Command, Arguments: append([]string(nil), request.Arguments...),
		LaneSessionID: laneID, Name: name, Cwd: cwd, PermissionMode: permission, NativeActor: existingActor,
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(normalized.Cwd) != "" {
		cwd = normalized.Cwd
	}
	if strings.TrimSpace(normalized.PermissionMode) != "" {
		permission = normalized.PermissionMode
	}
	input := map[string]any{"prompt": request.Input}
	input["options"] = map[string]any{
		"command":   request.Command,
		"arguments": append([]string(nil), options.productArguments...),
		"timeout":   options.timeout, "prompt_file": options.promptFile != "", "notify": options.notify,
		"no_notify":  options.noNotify,
		"persistent": options.persistent, "no_auto_archive": options.noAutoArchive,
		"auto_archive_after": options.autoArchiveAfter,
		"native":             cloneAttachmentEvidence(normalized.NativeOptions),
	}
	lane, turn, err := engine.Start(ctx, LaneStartRequest{
		LaneSessionID: laneID, TurnID: turnID, SourceAttachmentID: sourceAttachmentID,
		Product: request.Product, Name: name, Cwd: cwd, Groups: append([]string(nil), options.groups...),
		InheritParentGroups: options.inheritGroups, PermissionMode: permission, InputReference: input,
		AllowDuplicateName: options.allowDuplicateName,
	})
	if err != nil {
		if lane.LaneSessionID == "" && normalized.Rollback != nil {
			err = errors.Join(err, normalized.Rollback())
		}
		return nil, err
	}
	base["lane"], base["turn"] = publicLaneRecord(lane), publicLaneTurn(turn)
	if !wait {
		return base, nil
	}
	waitCtx, cancel, err := laneCommandWaitContext(ctx, options.timeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	collection, err := engine.Wait(waitCtx, LaneCollectRequest{
		LaneSessionID: lane.LaneSessionID, TurnID: turn.TurnID, SourceAttachmentID: sourceAttachmentID,
	})
	if err != nil {
		return nil, err
	}
	base["lane"], base["turn"], base["collection"] = publicLaneRecord(collection.Lane), publicLaneTurn(collection.Turn), publicLaneCollection(collection)
	return base, nil
}

func (engine *LaneEngine) normalizeLaneCommand(ctx context.Context, request LaneCommandNormalizationRequest) (LaneCommandNormalization, error) {
	engine.mu.Lock()
	adapter := engine.adapters[request.Product]
	engine.mu.Unlock()
	if normalizer, ok := adapter.(LaneCommandNormalizer); ok {
		return normalizer.NormalizeLaneCommand(ctx, request)
	}
	return LaneCommandNormalization{Cwd: request.Cwd, PermissionMode: request.PermissionMode}, nil
}

//nolint:gocyclo // One parser preserves the exact cross-product compatibility option surface.
func parseLaneCommandOptions(product, command string, arguments []string) (laneCommandOptions, []string, error) {
	var options laneCommandOptions
	parser := pflag.NewFlagSet("lane."+command, pflag.ContinueOnError)
	parser.SetOutput(io.Discard)
	parser.SetInterspersed(true)
	parser.StringVarP(&options.name, "name", "n", "", "lane name")
	parser.StringVar(&options.name, "peer-name", "", "lane name")
	parser.StringVarP(&options.cwd, "cd", "C", "", "working directory")
	parser.StringVar(&options.cwd, "cwd", "", "working directory")
	parser.StringVar(&options.timeout, "timeout", "", "timeout seconds")
	parser.StringVar(&options.promptFile, "prompt-file", "", "prompt file")
	parser.StringVar(&options.notify, "notify", "", "notification target")
	parser.BoolVar(&options.noNotify, "no-notify", false, "disable notification")
	parser.BoolVar(&options.noAutoArchive, "no-auto-archive", false, "disable automatic archive")
	parser.StringVar(&options.autoArchiveAfter, "auto-archive-after", "", "automatic archive delay")
	parser.BoolVar(&options.persistent, "persistent", false, "persistent lane")
	parser.StringArrayVarP(&options.groups, "group", "g", nil, "lane group")
	parser.BoolVar(&options.inheritGroups, "inherit-groups", false, "inherit parent groups")
	parser.BoolVar(&options.noInheritGroups, "no-inherit-groups", false, "do not inherit parent groups")
	parser.BoolVar(&options.allowDuplicateName, "allow-duplicate-name", false, "allow duplicate name")
	parser.BoolVar(&options.all, "all", false, "include archived lanes")
	parser.BoolVar(&options.mine, "mine", false, "only lanes owned by this parent")
	parser.BoolVar(&options.json, "json", false, "JSON output")
	var (
		model, effort, reasoningEffort, sandbox, approvalPolicy string
		schema, maxBudget, tools, allowedTools, disallowedTools string
		qwenHome, approvalMode, alwaysApprove, yolo             string
		configs                                                 []string
		web, noWeb                                              string
		worktree, bare, skipGitRepoCheck, noYolo                bool
	)
	parser.StringVarP(&model, "model", "m", "", "native model")
	parser.StringVar(&effort, "effort", "", "native reasoning effort")
	parser.StringVar(&reasoningEffort, "reasoning-effort", "", "native reasoning effort")
	parser.StringVar(&sandbox, "sandbox", "", "native sandbox policy")
	parser.StringVar(&approvalPolicy, "approval-policy", "", "native approval policy")
	parser.StringArrayVarP(&configs, "config", "c", nil, "native configuration override")
	parser.StringVar(&web, "web", "", "enable native web access")
	parser.Lookup("web").NoOptDefVal = "true"
	parser.StringVar(&noWeb, "no-web", "", "disable native web access")
	parser.Lookup("no-web").NoOptDefVal = "true"
	parser.StringVar(&schema, "schema", "", "native result schema")
	parser.BoolVar(&worktree, "worktree", false, "create a detached worktree")
	parser.BoolVar(&skipGitRepoCheck, "skip-git-repo-check", false, "compatibility option")
	parser.StringVar(&options.permissionMode, "permission-mode", "", "native permission mode")
	parser.StringVar(&maxBudget, "max-budget-usd", "", "native budget")
	parser.StringVar(&tools, "tools", "", "native tools")
	parser.StringVar(&allowedTools, "allowed-tools", "", "native allowed tools")
	parser.StringVar(&disallowedTools, "disallowed-tools", "", "native disallowed tools")
	parser.BoolVar(&bare, "bare", false, "native bare mode")
	parser.StringVar(&alwaysApprove, "always-approve", "", "native always-approve mode")
	parser.Lookup("always-approve").NoOptDefVal = "bypassPermissions"
	parser.StringVar(&yolo, "yolo", "", "native yolo mode")
	parser.Lookup("yolo").NoOptDefVal = "yolo"
	parser.BoolVar(&noYolo, "no-yolo", false, "disable native yolo mode")
	parser.StringVar(&approvalMode, "approval-mode", "", "native approval mode")
	parser.StringVar(&qwenHome, "qwen-home", "", "native Qwen profile")
	parser.BoolP("help", "h", false, "help")
	if err := parser.Parse(arguments); err != nil {
		return laneCommandOptions{}, nil, err
	}
	if options.inheritGroups && options.noInheritGroups {
		return laneCommandOptions{}, nil, errors.New("--inherit-groups and --no-inherit-groups are mutually exclusive")
	}
	if options.notify != "" && options.noNotify {
		return laneCommandOptions{}, nil, errors.New("--notify and --no-notify are mutually exclusive")
	}
	if options.notify != "" && !options.persistent && command != "resume" {
		return laneCommandOptions{}, nil, errors.New("--notify requires --persistent")
	}
	if options.autoArchiveAfter != "" && options.noAutoArchive {
		return laneCommandOptions{}, nil, errors.New("--auto-archive-after and --no-auto-archive are mutually exclusive")
	}
	if options.noInheritGroups {
		options.inheritGroups = false
	}
	positionals := make([]string, 0, len(parser.Args()))
	for _, value := range parser.Args() {
		if value != "-" {
			positionals = append(positionals, value)
		}
	}
	if err := validateLaneCommandOptionScope(command, parser); err != nil {
		return laneCommandOptions{}, nil, err
	}
	if err := validateLaneCommandProductOptions(product, command, parser, &options); err != nil {
		return laneCommandOptions{}, nil, err
	}
	options.productArguments = append([]string(nil), arguments...)
	return options, positionals, nil
}

func validateLaneCommandOptionScope(command string, parser *pflag.FlagSet) error {
	launch := map[string]bool{
		"name": true, "peer-name": true, "cd": true, "cwd": true, "timeout": true, "prompt-file": true,
		"notify": true, "no-notify": true, "persistent": true, "no-auto-archive": true,
		"auto-archive-after": true, "group": true, "inherit-groups": true, "no-inherit-groups": true,
		"allow-duplicate-name": true, "json": true, "help": true,
	}
	for _, name := range laneCommandProductOptionNames() {
		launch[name] = true
	}
	allowed := map[string]bool{"json": true, "help": true}
	switch command {
	case "run", "start", "resume":
		allowed = launch
	case "wait":
		allowed["timeout"] = true
	case "status", "interrupt", "archive":
		allowed["all"], allowed["mine"] = true, true
	case "list", "doctor":
		allowed["all"], allowed["mine"] = true, true
	default:
		return fmt.Errorf("unsupported lane lifecycle command %q", command)
	}
	var invalid string
	parser.Visit(func(flag *pflag.Flag) {
		if invalid == "" && !allowed[flag.Name] {
			invalid = "--" + flag.Name
		}
	})
	if invalid != "" {
		return fmt.Errorf("%s is not valid for lane %s", invalid, command)
	}
	return nil
}

//nolint:gocyclo // Product-specific option compatibility is deliberately validated in one fail-closed table-driven path.
func validateLaneCommandProductOptions(product, command string, parser *pflag.FlagSet, options *laneCommandOptions) error {
	allowed := map[string]map[string]bool{
		"codex":  laneCommandFlagSet("model", "effort", "reasoning-effort", "sandbox", "approval-policy", "config", "web", "no-web", "schema", "worktree", "skip-git-repo-check"),
		"claude": laneCommandFlagSet("model", "effort", "permission-mode", "max-budget-usd", "tools", "allowed-tools", "disallowed-tools", "schema", "bare", "worktree"),
		"grok":   laneCommandFlagSet("model", "effort", "reasoning-effort", "permission-mode", "always-approve", "yolo"),
		"qwen":   laneCommandFlagSet("qwen-home", "yolo", "no-yolo", "approval-mode"),
	}[product]
	productFlags := laneCommandFlagSet(laneCommandProductOptionNames()...)
	var invalid string
	parser.Visit(func(flag *pflag.Flag) {
		if invalid == "" && productFlags[flag.Name] && !allowed[flag.Name] {
			invalid = "--" + flag.Name
		}
	})
	if invalid != "" {
		return fmt.Errorf("%s is not valid for %s lanes", invalid, product)
	}
	if command == "resume" && (product == "codex" || product == "claude") {
		if changedLaneCommandFlag(parser, "worktree") {
			return errors.New("lane resume reuses its durable cwd and cannot create a worktree")
		}
	}
	if product == "claude" {
		bare, _ := parser.GetBool("bare")
		if bare {
			return errors.New("--bare is incompatible with messageable Claude lanes")
		}
		if options.permissionMode != "" && !laneCommandFlagSet("acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan")[options.permissionMode] {
			return fmt.Errorf("unsupported Claude permission mode %q", options.permissionMode)
		}
	}
	if product == "grok" {
		if changedLaneCommandFlag(parser, "always-approve") || changedLaneCommandFlag(parser, "yolo") {
			options.permissionMode = "bypassPermissions"
		}
		if options.permissionMode != "" && options.permissionMode != "bypassPermissions" {
			return fmt.Errorf("unsupported headless Grok permission mode %q; use bypassPermissions", options.permissionMode)
		}
	}
	if product == "qwen" {
		choices := 0
		for _, flag := range []string{"yolo", "no-yolo", "approval-mode"} {
			if changedLaneCommandFlag(parser, flag) {
				choices++
			}
		}
		if choices > 1 {
			return errors.New("qwen lane permission options are repeated or contradictory")
		}
		switch {
		case changedLaneCommandFlag(parser, "yolo"):
			options.permissionMode = "yolo"
		case changedLaneCommandFlag(parser, "no-yolo"):
			options.permissionMode = "default"
		case changedLaneCommandFlag(parser, "approval-mode"):
			options.permissionMode, _ = parser.GetString("approval-mode")
		}
		if options.permissionMode != "" && !laneCommandFlagSet("default", "yolo", "plan", "auto", "accept_edits")[options.permissionMode] {
			return fmt.Errorf("unsupported Qwen approval mode %q", options.permissionMode)
		}
		if changedLaneCommandFlag(parser, "qwen-home") {
			profile, _ := parser.GetString("qwen-home")
			if !filepath.IsAbs(strings.TrimSpace(profile)) {
				return errors.New("--qwen-home requires an absolute profile path")
			}
		}
	}
	if product == "codex" && changedLaneCommandFlag(parser, "sandbox") && changedLaneCommandFlag(parser, "approval-policy") {
		sandbox, _ := parser.GetString("sandbox")
		approval, _ := parser.GetString("approval-policy")
		if sandbox == "danger-full-access" && approval == "never" {
			options.permissionMode = "bypassPermissions"
		}
	}
	return nil
}

func laneCommandProductOptionNames() []string {
	return []string{
		"model", "effort", "reasoning-effort", "sandbox", "approval-policy", "config", "web", "no-web",
		"schema", "worktree", "skip-git-repo-check", "permission-mode", "max-budget-usd", "tools",
		"allowed-tools", "disallowed-tools", "bare", "always-approve", "yolo", "no-yolo", "approval-mode", "qwen-home",
	}
}

func laneCommandFlagSet(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func changedLaneCommandFlag(parser *pflag.FlagSet, name string) bool {
	flag := parser.Lookup(name)
	return flag != nil && flag.Changed
}

func resolveLaneCommandSelector(
	ctx context.Context,
	engine *LaneEngine,
	sourceAttachmentID, product, selector string,
	all, mine bool,
) (LaneRecord, error) {
	selector = strings.TrimSpace(selector)
	lanes, err := engine.List(ctx, LaneListRequest{SourceAttachmentID: sourceAttachmentID, All: all, Mine: mine})
	if err != nil {
		return LaneRecord{}, err
	}
	matches := make([]LaneRecord, 0, 1)
	for _, lane := range lanes {
		if lane.Product == product && (lane.LaneSessionID == selector || lane.Name == selector) {
			matches = append(matches, lane)
		}
	}
	if len(matches) == 0 {
		return LaneRecord{}, ErrLaneNotFound
	}
	if len(matches) > 1 {
		return LaneRecord{}, ErrLaneIdempotencyConflict
	}
	return matches[0], nil
}

func (engine *LaneEngine) latestLaneTurn(_ context.Context, laneSessionID, sourceAttachmentID string) (LaneTurnRecord, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	lane, ok := engine.lanes[laneSessionID]
	if !ok {
		return LaneTurnRecord{}, ErrLaneNotFound
	}
	if err := engine.authorizeLaneSource(lane, sourceAttachmentID); err != nil {
		return LaneTurnRecord{}, err
	}
	var selected *LaneTurnRecord
	for _, turn := range engine.turns {
		if turn.LaneSessionID != laneSessionID {
			continue
		}
		if selected == nil || turn.CreatedAt > selected.CreatedAt ||
			(turn.CreatedAt == selected.CreatedAt && turn.Revision > selected.Revision) {
			clonedTurn := cloneLaneTurnRecord(turn)
			selected = &clonedTurn
		}
	}
	if selected == nil {
		return LaneTurnRecord{}, ErrLaneNotFound
	}
	return *selected, nil
}

func resolveLaneCommandCwd(parentCwd, requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return filepath.Clean(parentCwd)
	}
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(parentCwd, requested)
	}
	return filepath.Clean(requested)
}

func laneCommandPermission(product, parent, explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit)
	}
	switch product {
	case "claude":
		if parent == "bypassPermissions" {
			return parent
		}
		return "dontAsk"
	case "grok":
		return "bypassPermissions"
	case "qwen":
		if parent == "bypassPermissions" || parent == "yolo" {
			return parent
		}
		return "default"
	default:
		return strings.TrimSpace(parent)
	}
}

func laneCommandWaitContext(ctx context.Context, value string) (context.Context, context.CancelFunc, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		waitCtx, cancel := context.WithCancel(ctx)
		return waitCtx, cancel, nil
	}
	seconds, err := time.ParseDuration(value + "s")
	if err != nil || seconds < 0 {
		return nil, nil, errors.New("lane --timeout must be a non-negative number of seconds")
	}
	waitCtx, cancel := context.WithTimeout(ctx, seconds)
	return waitCtx, cancel, nil
}

func supportedLaneCommandProduct(product string) bool {
	for _, value := range []string{"codex", "claude", "grok", "qwen"} {
		if product == value {
			return true
		}
	}
	return false
}

func publicLaneRecord(record LaneRecord) LaneRecord {
	return cloneLaneRecord(record)
}

func publicLaneTurn(record LaneTurnRecord) LaneTurnRecord {
	result := cloneLaneTurnRecord(record)
	result.InputReference = nil
	result.RequestDigest = ""
	return result
}

func publicLaneCollection(collection LaneCollection) LaneCollection {
	result := collection
	result.Lane = publicLaneRecord(collection.Lane)
	result.Turn = publicLaneTurn(collection.Turn)
	result.ResultReference = cloneAttachmentEvidence(collection.ResultReference)
	return result
}
