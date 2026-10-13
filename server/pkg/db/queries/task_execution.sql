-- name: RecordAgentTaskExecutionExit :one
-- The dispatch fence also guards revocation against a delayed callback.
-- Include task identity because retries can inherit their parent's context.
WITH exited AS (
    UPDATE agent_task_queue
    SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object(
        'execution_exit_task_id', agent_task_queue.id::text,
        'execution_exited_at', COALESCE(
            CASE WHEN context->>'execution_exit_task_id' = agent_task_queue.id::text
                 THEN context->>'execution_exited_at' END,
            now()::text))
    WHERE agent_task_queue.id = sqlc.arg(id)
      AND runtime_id IS NOT DISTINCT FROM sqlc.narg(runtime_id)::uuid
      AND dispatched_at IS NOT DISTINCT FROM sqlc.narg(dispatched_at)::timestamptz
      AND status IN ('completed', 'failed', 'cancelled')
    RETURNING agent_task_queue.*
), revoked AS (
    DELETE FROM task_token USING exited WHERE task_token.task_id = exited.id
)
SELECT * FROM exited;
