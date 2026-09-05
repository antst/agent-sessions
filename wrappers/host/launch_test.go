package host

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLaneModeUsesTokenPresence(t *testing.T) {
	t.Setenv(TokenEnv, "")
	if !LaneMode() {
		t.Fatal("empty present token did not select lane mode")
	}
	if err := os.Unsetenv(TokenEnv); err != nil {
		t.Fatal(err)
	}
	if LaneMode() {
		t.Fatal("absent token selected lane mode")
	}
}

func TestInteractivePlan(t *testing.T) {
	plan, err := InteractivePlan("claude", []string{
		"--model", "-g", "-g", "project", "--group=review", "--", "-g", "literal",
	}, []string{"PATH=/bin", GroupsEnv + "=[\"old\"]"}, PeerIdentity{SessionID: "id", Name: "name"}, func(option string) bool { return option == "--model" })
	must(t, err)
	wantArgs := []string{"--model", "-g", "--", "-g", "literal"}
	if !reflect.DeepEqual(plan.Args, wantArgs) {
		t.Fatalf("args = %q", plan.Args)
	}
	joined := "\n" + strings.Join(plan.Env, "\n") + "\n"
	for _, want := range []string{"\nAGENTBUS_SESSION_ID=id\n", "\nAGENTBUS_SESSION_NAME=name\n", "\nAGENTBUS_GROUPS=[\"project\",\"review\"]\n"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("environment missing %q: %s", want, joined)
		}
	}
}

func TestInteractivePlanErrors(t *testing.T) {
	for _, arguments := range [][]string{{"-g"}, {"--group="}} {
		if _, err := InteractivePlan("product", arguments, nil, PeerIdentity{}, nil); err == nil {
			t.Fatalf("arguments %q succeeded", arguments)
		}
	}
}

func TestBuildArgumentsTable(t *testing.T) {
	rules := []ArgumentRule{
		{Name: "--agent", TakesValue: true},
		{Name: "--model", TakesValue: true, ConflictField: "model"},
		{Name: "-c", TakesValue: true, Conflict: func(value string) string {
			if strings.HasPrefix(value, "model=") {
				return "model"
			}
			return ""
		}},
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"ordered", []string{"--agent", "one", "--agent=two", "-c", "web=true"}, ""},
		{"unknown", []string{"--other"}, "unsupported argument --other"},
		{"missing", []string{"--agent"}, "--agent requires a value"},
		{"typed", []string{"--model", "x"}, "argument conflicts with typed field model"},
		{"dynamic typed", []string{"-c=model=x"}, "argument conflicts with typed field model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := BuildArguments(test.args, rules)
			if test.want == "" && (err != nil || !reflect.DeepEqual(got, test.args)) {
				t.Fatalf("got %q, %v", got, err)
			}
			if test.want != "" && (err == nil || err.Error() != test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
