package daemon

import (
	"path/filepath"
	"testing"

	"github.com/antst/agent-sessions/internal/testutil"
)

func TestAggregateResourceAndDependencyPreAcceptanceGates(t *testing.T) {
	root := testutil.ShortSocketRoot(t, "r90-", filepath.Join("run", "daemon.sock"))
	gates := []aggregateGoTestGate{
		{
			name:        "host disk memory file-descriptor and process failures",
			packagePath: "./internal/daemon",
			tests: []string{
				"TestRuntimeLifecycleResourceExhaustionFailsBeforeAdmission",
				"TestDeliveryResourceBudgetsFailBeforeAcceptanceOrNativeWork",
				"TestLaneResourceFailuresRejectBeforeDurableAcceptanceOrNativeDispatch",
				"TestNativeConnectorDriversTreatEveryMissingProductAsOptional",
			},
		},
		{
			name:        "native dependency failure before managed mutation",
			packagePath: "./internal/launcher",
			tests: []string{
				"TestProductExecutableEnvironmentRejectsMissingOverride",
				"TestRunQwenPeerReadinessPrecedesManagedMutation",
			},
		},
		{
			name:        "hub resource failure and durable acceptance",
			packagePath: "./internal/federation",
			tests: []string{
				"TestHubResourceFailuresRejectBeforeDurableRegistrationOrWorkAcceptance",
				"TestHubAdmissionReportsSuccessOnlyAfterDurableCommit",
			},
		},
	}
	for _, gate := range gates {
		t.Run(gate.name, func(t *testing.T) {
			runExistingGoTestGate(t, root, gate)
		})
	}
}
