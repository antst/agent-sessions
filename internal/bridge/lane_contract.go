package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

var laneCommands = []string{"run", "start", "resume", "wait", "status", "interrupt", "archive", "list", "doctor"}

type laneOptionCheck struct {
	flag string
	set  bool
}

func laneOption(flag string, set bool) laneOptionCheck {
	return laneOptionCheck{flag: flag, set: set}
}

func validateLaneCommandOptions(command string, checks []laneOptionCheck) error {
	for _, check := range checks {
		if check.set {
			return fmt.Errorf("%s is not valid for %s", check.flag, command)
		}
	}
	return nil
}

// laneCommonOptions is the product-independent lane CLI contract. Product
// option structs embed it so lifecycle, grouping, selection, and collection
// flags have one parser and one validator across every adapter.
type laneCommonOptions struct {
	command            string
	name               string
	nameSet            bool
	target             string
	cwd                string
	cwdSet             bool
	timeout            time.Duration
	timeoutSet         bool
	promptFile         string
	notifyTarget       string
	notifyExplicit     bool
	disableNotify      bool
	persistent         bool
	persistentSet      bool
	autoArchive        bool
	noAutoArchiveSet   bool
	autoArchiveDelay   time.Duration
	autoArchiveCustom  bool
	ownerPID           int
	ownerProcStart     string
	ownerSessionID     string
	targetLabel        string
	allowDuplicateName bool
	all                bool
	mine               bool
	json               bool
	stdinMarker        bool
	help               bool
	groupOptions       laneGroupOptions
}

func newLaneCommonOptions(targetLabel string) laneCommonOptions {
	return laneCommonOptions{
		cwd: mustGetwd(), autoArchive: true, autoArchiveDelay: defaultLaneAutoArchiveDelay,
		targetLabel: targetLabel,
	}
}

func beginLaneOptionParse(argv []string, common *laneCommonOptions) (int, bool, error) {
	if len(argv) == 0 {
		common.help = true
		return 0, true, nil
	}
	for _, argument := range argv {
		if argument == "-h" || argument == "--help" {
			common.help = true
			return 0, true, nil
		}
	}
	common.command = argv[0]
	if !containsString(laneCommands, common.command) {
		return 0, false, fmt.Errorf("unknown command %q", common.command)
	}
	return 1, false, nil
}

type laneFlagParser struct {
	set             *pflag.FlagSet
	common          *laneCommonOptions
	inheritGroups   bool
	noInheritGroups bool
}

func newLaneFlagParser(binary string, common *laneCommonOptions) *laneFlagParser {
	set := pflag.NewFlagSet(binary, pflag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.SetInterspersed(true)
	parser := &laneFlagParser{set: set, common: common}
	set.StringVarP(&common.name, "name", "n", common.name, "lane name")
	set.StringVar(&common.name, "peer-name", common.name, "lane name compatibility alias")
	set.StringVarP(&common.cwd, "cd", "C", common.cwd, "working directory")
	set.StringVar(&common.cwd, "cwd", common.cwd, "working directory alias")
	set.Var(newLaneSecondsFlag(&common.timeout, false, "--timeout", &common.timeoutSet), "timeout", "seconds")
	set.StringVar(&common.promptFile, "prompt-file", common.promptFile, "prompt file")
	set.StringVar(&common.notifyTarget, "notify", common.notifyTarget, "notification target")
	set.BoolVar(&common.disableNotify, "no-notify", common.disableNotify, "disable owner notification")
	set.BoolVar(&common.persistent, "persistent", common.persistent, "survive owner exit")
	set.BoolVar(&common.noAutoArchiveSet, "no-auto-archive", common.noAutoArchiveSet, "disable auto archive")
	set.Var(newLaneSecondsFlag(&common.autoArchiveDelay, true, "--auto-archive-after", &common.autoArchiveCustom), "auto-archive-after", "seconds")
	set.StringArrayVarP(&common.groupOptions.groups, "group", "g", common.groupOptions.groups, "child group")
	set.BoolVar(&parser.inheritGroups, "inherit-groups", false, "inherit parent groups")
	set.BoolVar(&parser.noInheritGroups, "no-inherit-groups", false, "do not inherit parent groups")
	set.BoolVar(&common.allowDuplicateName, "allow-duplicate-name", common.allowDuplicateName, "allow a duplicate active name")
	set.BoolVar(&common.all, "all", common.all, "include archived lanes")
	set.BoolVar(&common.mine, "mine", common.mine, "only lanes owned by this parent")
	set.BoolVar(&common.json, "json", common.json, "emit JSON")
	_ = set.MarkHidden("peer-name")
	_ = set.MarkHidden("allow-duplicate-name")
	return parser
}

func (p *laneFlagParser) parse(argv []string) ([]string, error) {
	if err := p.set.Parse(argv); err != nil {
		return nil, err
	}
	p.common.nameSet = p.set.Changed("name") || p.set.Changed("peer-name")
	p.common.cwdSet = p.set.Changed("cd") || p.set.Changed("cwd")
	p.common.notifyExplicit = p.set.Changed("notify")
	p.common.persistentSet = p.set.Changed("persistent")
	p.common.noAutoArchiveSet = p.set.Changed("no-auto-archive")
	p.common.groupOptions.groupsSpecified = p.set.Changed("group")
	inheritSet := p.set.Changed("inherit-groups")
	noInheritSet := p.set.Changed("no-inherit-groups")
	if inheritSet && noInheritSet {
		return nil, errors.New("--inherit-groups and --no-inherit-groups cannot be used together")
	}
	if inheritSet || noInheritSet {
		p.common.groupOptions.inheritGroupsSpecified = true
		p.common.groupOptions.inheritParentGroups = inheritSet && p.inheritGroups
	}
	if p.common.noAutoArchiveSet {
		p.common.autoArchive = false
	}
	positionals := make([]string, 0, p.set.NArg())
	for _, argument := range p.set.Args() {
		if argument == "-" {
			p.common.stdinMarker = true
			continue
		}
		positionals = append(positionals, argument)
	}
	return positionals, nil
}

type laneSecondsFlag struct {
	destination *time.Duration
	positive    bool
	flag        string
	changed     *bool
}

func newLaneSecondsFlag(destination *time.Duration, positive bool, flag string, changed *bool) *laneSecondsFlag {
	return &laneSecondsFlag{destination: destination, positive: positive, flag: flag, changed: changed}
}

// Set implements pflag.Value.
func (f *laneSecondsFlag) Set(value string) error {
	parsed, err := parseLaneSeconds(value, f.positive, f.flag)
	if err != nil {
		return err
	}
	*f.destination = parsed
	*f.changed = true
	return nil
}

func (f *laneSecondsFlag) String() string {
	if f == nil || f.destination == nil {
		return "0"
	}
	return strconv.FormatFloat(f.destination.Seconds(), 'f', -1, 64)
}

// Type implements pflag.Value.
func (*laneSecondsFlag) Type() string { return "seconds" }

type laneChoiceFlag struct {
	destination *string
	fixed       string
	count       *int
}

// Set implements pflag.Value.
func (f *laneChoiceFlag) Set(value string) error {
	*f.count++
	if f.fixed != "" {
		*f.destination = f.fixed
	} else {
		*f.destination = value
	}
	return nil
}

func (f *laneChoiceFlag) String() string {
	if f == nil || f.destination == nil {
		return ""
	}
	return *f.destination
}

// Type implements pflag.Value.
func (*laneChoiceFlag) Type() string { return "value" }

func validateLaneCommonOptions(common *laneCommonOptions, positionals []string) error {
	if err := validateLaneCommonOptionConflicts(common); err != nil {
		return err
	}
	if err := validateLaneGroupCommand(common.command, common.groupOptions); err != nil {
		return err
	}
	return validateLanePositionals(common, positionals)
}

func validateLaneCommonOptionConflicts(common *laneCommonOptions) error {
	if common.notifyTarget != "" && common.disableNotify {
		return errors.New("--notify and --no-notify cannot be used together")
	}
	if common.notifyExplicit && !common.persistent && common.command != "resume" {
		return errors.New("--notify requires --persistent; parent-owned lanes notify their owner automatically")
	}
	if common.autoArchiveCustom && !common.autoArchive {
		return errors.New("--auto-archive-after and --no-auto-archive cannot be used together")
	}
	if common.autoArchiveCustom && !containsString([]string{"run", "start", "resume"}, common.command) {
		return fmt.Errorf("--auto-archive-after is not valid for %s", common.command)
	}
	if common.mine && common.command != "list" {
		return fmt.Errorf("--mine is not valid for %s", common.command)
	}
	return nil
}

func validateLanePositionals(common *laneCommonOptions, positionals []string) error {
	switch common.command {
	case "run", "start":
		if strings.TrimSpace(common.name) == "" {
			return fmt.Errorf("%s requires --name", common.command)
		}
		if len(positionals) != 0 {
			return fmt.Errorf("%s does not accept a prompt on argv; use stdin or --prompt-file", common.command)
		}
	case "resume":
		if len(positionals) != 1 {
			return fmt.Errorf("resume requires exactly one %s", common.targetLabel)
		}
		common.target = positionals[0]
	case "list", "doctor":
		if len(positionals) != 0 {
			return fmt.Errorf("%s does not accept positional arguments", common.command)
		}
	default:
		if len(positionals) != 1 {
			return fmt.Errorf("%s requires exactly one %s", common.command, common.targetLabel)
		}
		common.target = positionals[0]
	}
	return nil
}

func laneCommandNeedsParent(common laneCommonOptions) bool {
	return containsString([]string{"run", "start", "resume"}, common.command) ||
		(common.command == "list" && common.mine)
}

func withCurrentLaneParent(common laneCommonOptions) laneCommonOptions {
	if !laneCommandNeedsParent(common) {
		return common
	}
	return withResolvedLaneParent(common, inferPeerParent(resolveNativePaths(), os.Getpid()))
}

func withResolvedLaneParent(common laneCommonOptions, owner laneOwner) laneCommonOptions {
	listMine := common.command == "list" && common.mine
	common.groupOptions = applyAgentParentContext(common.groupOptions, &owner)
	if owner.SessionID == "" {
		return common
	}
	common.groupOptions.parentSessionID = owner.SessionID
	if !common.persistent || listMine {
		common.ownerPID, common.ownerProcStart, common.ownerSessionID = owner.PID, owner.ProcStart, owner.SessionID
	}
	if !listMine && !common.persistent && !common.disableNotify && !common.notifyExplicit {
		common.notifyTarget = "session:" + owner.SessionID
	}
	return common
}

type productLaneCommands[T any] struct {
	binary    string
	usage     func() string
	parse     func([]string) (T, error)
	parseExit int
	help      func(T) bool
	prepare   func(T) (T, error)
	command   func(T) string
	start     func(T, bool) (int, error)
	resume    func(T) (int, error)
	wait      func(T) (int, error)
	status    func(T) (int, error)
	interrupt func(T) (int, error)
	archive   func(T) (int, error)
	list      func(T) (int, error)
	doctor    func(T) (int, error)
}

func runProductLaneCommand[T any](argv []string, product productLaneCommands[T]) int {
	options, err := product.parse(argv)
	if err != nil {
		return reportLaneCommandError(product.binary, err, product.parseExit, false)
	}
	if product.help(options) {
		fmt.Print(product.usage())
		return 0
	}
	options, err = product.prepare(options)
	if err != nil {
		return reportLaneCommandError(product.binary, err, 1, true)
	}
	var code int
	switch command := product.command(options); command {
	case "run":
		code, err = product.start(options, true)
	case "start":
		code, err = product.start(options, false)
	case "resume":
		code, err = product.resume(options)
	case "wait":
		code, err = product.wait(options)
	case "status":
		code, err = product.status(options)
	case "interrupt":
		code, err = product.interrupt(options)
	case "archive":
		code, err = product.archive(options)
	case "list":
		code, err = product.list(options)
	case "doctor":
		code, err = product.doctor(options)
	default:
		err = fmt.Errorf("unknown lane command %q", command)
	}
	if err != nil {
		return reportLaneCommandError(product.binary, err, 1, true)
	}
	return code
}

func reportLaneCommandError(binary string, err error, fallbackExit int, includeTimeout bool) int {
	result := map[string]any{"type": "error", "message": err.Error()}
	if includeTimeout {
		result["timeout"] = errors.Is(err, context.DeadlineExceeded)
	}
	_ = emitLane(result)
	fmt.Fprintf(os.Stderr, "%s: %v\n", binary, err)
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	return fallbackExit
}

func parseLaneSeconds(value string, positive bool, flag string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	minimum := 0.0
	message := flag + " must be a non-negative number of seconds"
	if positive {
		minimum = 0.001
		message = flag + " must be at least 0.001 seconds"
	}
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < minimum ||
		seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, errors.New(message)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func readProductLaneStates[T any](
	directory string,
	valid func(entryName string, state *T) bool,
	createdAt func(state *T) int64,
) []T {
	entries, _ := os.ReadDir(directory)
	states := make([]T, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // entry comes from a bridge-owned state directory.
		var state T
		if err != nil || json.Unmarshal(body, &state) != nil || !valid(entry.Name(), &state) {
			continue
		}
		states = append(states, state)
	}
	if createdAt != nil {
		sort.Slice(states, func(i, j int) bool { return createdAt(&states[i]) > createdAt(&states[j]) })
	}
	return states
}

func resolveProductLaneState[T any](
	target string,
	states []T,
	idMatches func(*T, string) bool,
	name func(*T) string,
	status func(*T) string,
	productLabel string,
	idLabel string,
) (T, error) {
	target = strings.TrimSpace(target)
	byName := make([]T, 0)
	for index := range states {
		state := &states[index]
		if idMatches(state, target) {
			return *state, nil
		}
		if strings.EqualFold(name(state), target) {
			byName = append(byName, *state)
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		active := make([]T, 0, len(byName))
		for index := range byName {
			if status(&byName[index]) != "archived" {
				active = append(active, byName[index])
			}
		}
		if len(active) == 1 {
			return active[0], nil
		}
		var zero T
		return zero, fmt.Errorf("%s lane name %q is ambiguous; use a %s", strings.ToLower(productLabel), target, idLabel)
	}
	var zero T
	return zero, fmt.Errorf("no %s lane matching %q", productLabel, target)
}

func listProductLaneStates[T any](
	common laneCommonOptions,
	product string,
	contractVersion int,
	states []T,
	status func(*T) string,
	ownership func(*T) (persistent bool, ownerPID int, ownerProcStart string),
	event func(T) map[string]any,
) (int, error) {
	if common.mine && !validLaneOwner(common.ownerPID, common.ownerProcStart) {
		return 1, errors.New("cannot establish the current orchestrator identity for --mine")
	}
	rows := make([]map[string]any, 0, len(states))
	for index := range states {
		state := &states[index]
		if !common.all && status(state) == "archived" {
			continue
		}
		persistent, ownerPID, ownerProcStart := ownership(state)
		if common.mine && (persistent || !sameLaneOwner(ownerPID, ownerProcStart, common.ownerPID, common.ownerProcStart)) {
			continue
		}
		row := event(*state)
		delete(row, "type")
		rows = append(rows, row)
	}
	return 0, emitLane(map[string]any{
		"type": "lane.list", "product": product, "contract_version": contractVersion, "lanes": rows,
	})
}

func waitProductLaneReady[T any](
	managerPID int,
	managerProcStart string,
	timeout time.Duration,
	read func() (T, error),
	ready func(*T) bool,
	exitedError string,
	timeoutError string,
) (T, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := read()
		if err == nil && ready(&state) {
			return state, nil
		}
		if cleanupProcessIdentityStatus(managerPID, managerProcStart).Status == processIdentityStale {
			var zero T
			return zero, errors.New(exitedError)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var zero T
	return zero, errors.New(timeoutError)
}

func appendLaneTerminalNotice(
	notices []claudeLaneNotice,
	product string,
	name string,
	sessionID string,
	turnID string,
	status string,
	outcome string,
	exit int,
	target string,
	parentHostID string,
	parentAgentRuntimeDir string,
	groups []string,
) []claudeLaneNotice {
	if target == "" {
		return notices
	}
	for _, notice := range notices {
		if notice.TurnID == turnID {
			return notices
		}
	}
	noticeID := sessionKey(product + "-lane-terminal\x00" + sessionID + "\x00" + turnID)
	collect := laneCollectionPointer(product, sessionID, parentHostID, parentAgentRuntimeDir, groups)
	message := fmt.Sprintf(
		"%s_LANE_TERMINAL notice=%s name=%s session=%s turn=%s status=%s outcome=%s exit=%d collection=required\nCollect: %s",
		strings.ToUpper(product), noticeID, name, sessionID, turnID, status, outcome, exit, collect,
	)
	return append(notices, claudeLaneNotice{
		ID: noticeID, TurnID: turnID, Target: target, Message: message, CreatedAt: time.Now().UnixMilli(),
	})
}

func acceptLaneControlLoop(
	listener net.Listener,
	done <-chan struct{},
	begin func() bool,
	finish func(),
	handle func(net.Conn),
) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-done:
				return
			default:
				continue
			}
		}
		if !begin() {
			_ = conn.Close()
			continue
		}
		go func() {
			defer finish()
			handle(conn)
		}()
	}
}
