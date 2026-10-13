package daemon

import "context"

type executionExitContextKey struct{}

type executionExitClaim struct {
	taskID string
	proof  string
}

func withExecutionExitProof(ctx context.Context, taskID, proof string) context.Context {
	return context.WithValue(ctx, executionExitContextKey{}, executionExitClaim{taskID: taskID, proof: proof})
}

func executionExitProofForTask(ctx context.Context, taskID string) string {
	claim, _ := ctx.Value(executionExitContextKey{}).(executionExitClaim)
	if claim.taskID == taskID {
		return claim.proof
	}
	return ""
}

func executionExitProofForPath(ctx context.Context, path string) string {
	claim, _ := ctx.Value(executionExitContextKey{}).(executionExitClaim)
	prefix := "/api/daemon/tasks/" + claim.taskID
	if path == prefix+"/complete" || path == prefix+"/fail" || path == prefix+"/cancel-ack" {
		return claim.proof
	}
	return ""
}
