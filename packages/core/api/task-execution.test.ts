// @vitest-environment node
import { expect, test } from 'vitest';
import { AgentTaskSchema } from './schemas';
test('execution exit metadata preserves old clients and ignores malformed optional evidence', () => {
  expect(AgentTaskSchema.parse({ id: 'run' }).execution_exited_at).toBeUndefined();
  expect(AgentTaskSchema.parse({ id: 'run', execution_exited_at: 42 }).execution_exited_at).toBeUndefined();
  expect(AgentTaskSchema.parse({ id: 'run', execution_exited_at: '2026-10-09T12:00:00Z' }).execution_exited_at).toBe('2026-10-09T12:00:00Z');
});
