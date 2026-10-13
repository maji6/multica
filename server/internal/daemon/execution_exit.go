package daemon

import (
	"context"
	"sync/atomic"
)

type executionExitContextKey struct{}

type executionExitClaim struct {
	taskID      string
	proof       string
	unconfirmed atomic.Bool
}

func withExecutionExitProof(ctx context.Context, taskID, proof string) context.Context {
	return context.WithValue(ctx, executionExitContextKey{}, &executionExitClaim{taskID: taskID, proof: proof})
}

func executionExitProofForTask(ctx context.Context, taskID string) string {
	claim, _ := ctx.Value(executionExitContextKey{}).(*executionExitClaim)
	if claim != nil && claim.taskID == taskID && !claim.unconfirmed.Load() {
		return claim.proof
	}
	return ""
}

func executionExitProofForPath(ctx context.Context, path string) string {
	claim, _ := ctx.Value(executionExitContextKey{}).(*executionExitClaim)
	if claim == nil || claim.unconfirmed.Load() {
		return ""
	}
	prefix := "/api/daemon/tasks/" + claim.taskID
	if path == prefix+"/complete" || path == prefix+"/fail" || path == prefix+"/cancel-ack" {
		return claim.proof
	}
	return ""
}

// Only a successfully drained provider result can clear this fence after a
// session starts. Timeout/business cancellation alone cannot prove cleanup.
func executionExitClaimForTask(ctx context.Context, taskID string) *executionExitClaim {
	claim, _ := ctx.Value(executionExitContextKey{}).(*executionExitClaim)
	if claim != nil && claim.taskID == taskID && claim.proof != "" {
		return claim
	}
	return nil
}
