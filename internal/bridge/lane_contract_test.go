package bridge

import (
	"reflect"
	"testing"
	"time"
)

func TestAllLaneParsersShareLifecycleAndGroupContract(t *testing.T) {
	args := []string{
		"start", "--name", "worker", "-C", "/tmp", "-g", "project",
		"--inherit-groups", "--persistent", "--notify", "coordinator",
		"--auto-archive-after", "2.5", "--timeout", "3", "-",
	}
	type projection struct {
		name, cwd, notify string
		groups            laneGroupOptions
		persistent        bool
		autoArchive       time.Duration
		timeout           time.Duration
	}
	project := func(common laneCommonOptions) projection {
		return projection{
			name: common.name, cwd: common.cwd, notify: common.notifyTarget,
			groups: common.groupOptions, persistent: common.persistent,
			autoArchive: common.autoArchiveDelay, timeout: common.timeout,
		}
	}
	codex, err := parseLaneArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	claude, err := parseClaudeLaneArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	grok, err := parseGrokLaneArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	qwen, err := parseQwenLaneArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	want := project(codex.laneCommonOptions)
	for product, got := range map[string]projection{
		"claude": project(claude.laneCommonOptions),
		"grok":   project(grok.laneCommonOptions),
		"qwen":   project(qwen.laneCommonOptions),
	} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s common lane options = %#v, want %#v", product, got, want)
		}
	}
}

func TestPersistentDoesNotDisableAutoArchiveAcrossProducts(t *testing.T) {
	parsers := map[string]func([]string) (laneCommonOptions, error){
		"codex": func(args []string) (laneCommonOptions, error) {
			options, err := parseLaneArgs(args)
			return options.laneCommonOptions, err
		},
		"claude": func(args []string) (laneCommonOptions, error) {
			options, err := parseClaudeLaneArgs(args)
			return options.laneCommonOptions, err
		},
		"grok": func(args []string) (laneCommonOptions, error) {
			options, err := parseGrokLaneArgs(args)
			return options.laneCommonOptions, err
		},
		"qwen": func(args []string) (laneCommonOptions, error) {
			options, err := parseQwenLaneArgs(args)
			return options.laneCommonOptions, err
		},
	}
	for product, parse := range parsers {
		t.Run(product, func(t *testing.T) {
			persistent, err := parse([]string{"start", "--name", "worker", "--persistent", "-"})
			if err != nil || !persistent.persistent || !persistent.autoArchive || persistent.autoArchiveDelay != time.Minute {
				t.Fatalf("persistent lifecycle = %+v, %v", persistent, err)
			}
			retained, err := parse([]string{"start", "--name", "worker", "--persistent", "--no-auto-archive", "-"})
			if err != nil || !retained.persistent || retained.autoArchive {
				t.Fatalf("retained lifecycle = %+v, %v", retained, err)
			}
		})
	}
}

func TestResolvedLaneParentPreservesExplicitNotification(t *testing.T) {
	common := laneCommonOptions{
		command: "resume", notifyTarget: "session:explicit", notifyExplicit: true,
	}
	owner := laneOwner{PID: 42, ProcStart: "start", SessionID: "owner"}
	got := withResolvedLaneParent(common, owner)
	if got.notifyTarget != "session:explicit" || got.ownerSessionID != "owner" {
		t.Fatalf("resolved parent = %+v", got)
	}
}

func TestParseLaneSecondsRejectsNonFiniteAndOverflow(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "9.223372036854776e9"} {
		if _, err := parseLaneSeconds(value, false, "--timeout"); err == nil {
			t.Errorf("parseLaneSeconds(%q) accepted an invalid duration", value)
		}
	}
	if got, err := parseLaneSeconds("0.001", true, "--auto-archive-after"); err != nil || got != time.Millisecond {
		t.Fatalf("minimum positive duration = %v, %v", got, err)
	}
}
