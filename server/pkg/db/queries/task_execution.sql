-- name: RecordAgentTaskExecutionExit :one
-- Only a daemon terminal callback may produce this receipt. Include the task
-- identity because retry tasks can inherit their parent's context JSON.
WITH revoked AS (
    DELETE FROM task_token WHERE task_id = $1
)
UPDATE agent_task_queue
SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object(
    'execution_exit_task_id', agent_task_queue.id::text,
    'execution_exited_at', COALESCE(
        CASE WHEN context->>'execution_exit_task_id' = agent_task_queue.id::text
             THEN context->>'execution_exited_at' END,
        now()::text))
WHERE agent_task_queue.id = $1 AND status IN ('completed', 'failed', 'cancelled')
RETURNING agent_task_queue.*;
