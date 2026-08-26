CREATE TABLE task_executions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  title TEXT,                     -- task title at dispatch time
  task_number TEXT,               -- human reference, e.g. "ERP-42"
  task_url TEXT,                  -- link to the task in its source UI
  model TEXT,                     -- LLM model used for this attempt
  runner TEXT,                    -- runner type (cli, script, …)
  attempt INTEGER DEFAULT 1,
  status TEXT,                    -- pending, running, success, failed
  started_at TIMESTAMP,
  completed_at TIMESTAMP,
  duration_ms INTEGER,
  error_message TEXT,
  can_retry BOOLEAN,
  next_retry_at TIMESTAMP,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, pid INTEGER, heartbeat_at TIMESTAMP, heartbeat_count INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, total_tokens INTEGER DEFAULT 0, cache_creation_tokens INTEGER DEFAULT 0, cache_read_tokens INTEGER DEFAULT 0, num_turns INTEGER DEFAULT 0, num_tool_calls INTEGER DEFAULT 0, cost_usd REAL DEFAULT 0.0, workflow_instance_id TEXT, step_id TEXT, input_prompt TEXT, output_text TEXT, credit_exhausted INTEGER NOT NULL DEFAULT 0, failure_kind TEXT, time_thinking_ms INTEGER NOT NULL DEFAULT 0, time_writing_ms INTEGER NOT NULL DEFAULT 0, time_model_ms INTEGER NOT NULL DEFAULT 0, time_tool_wait_ms INTEGER NOT NULL DEFAULT 0, time_other_ms INTEGER NOT NULL DEFAULT 0, time_background_ms INTEGER NOT NULL DEFAULT 0, slow_tools TEXT,
  FOREIGN KEY(task_id) REFERENCES tasks(id),
  FOREIGN KEY(agent_id) REFERENCES agents(id)
);
CREATE TABLE task_checkpoints (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  attempt INTEGER,
  stage TEXT,                     -- initialized, running, completed
  metadata TEXT,                  -- JSON state data
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);
CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  source_id TEXT,
  title TEXT,
  agent_id TEXT,
  state TEXT,                     -- pending, running, completed, failed
  started_at TIMESTAMP,
  completed_at TIMESTAMP,
  duration_ms INTEGER,
  success BOOLEAN,
  output TEXT,
  full_output TEXT,
  error_message TEXT,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(agent_id) REFERENCES agents(id)
);
CREATE TABLE agents (
  id TEXT PRIMARY KEY,
  description TEXT,
  status TEXT,                    -- active, idle, error
  current_task_id TEXT,
  queued_count INTEGER DEFAULT 0,
  total_completed INTEGER DEFAULT 0,
  avg_duration_ms INTEGER DEFAULT 0,
  success_rate REAL DEFAULT 0.0,
  last_task_ended_at TIMESTAMP,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE task_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT,
  level TEXT,                     -- DEBUG, INFO, WARN, ERROR
  message TEXT,
  timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(task_id) REFERENCES tasks(id)
);
CREATE TABLE service_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  level TEXT,                     -- DEBUG, INFO, WARN, ERROR
  message TEXT,
  component TEXT,                 -- dispatcher, router, runner, etc.
  timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE execution_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  schema_version INTEGER NOT NULL,
  type TEXT NOT NULL,
  timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  task_id TEXT,
  workflow_id TEXT,
  workflow_instance_id TEXT,
  step_id TEXT,
  attempt_id TEXT,
  metadata TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE dispatcher_state (
  id INTEGER PRIMARY KEY,
  status TEXT,                    -- healthy, degraded, error
  uptime_seconds INTEGER,
  version TEXT,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE workflow_instances (
  id TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL,
  cell_id TEXT NOT NULL,
  source_id TEXT,
  state TEXT NOT NULL,            -- pending|running|approval_waiting|interrupted|done|failed
  parent_instance_id TEXT,       -- set for sub-workflow child instances
  resumed_from TEXT,             -- instance id this was resumed from
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
, task_id TEXT REFERENCES internal_tasks(id), task_generation INTEGER NOT NULL DEFAULT 0);
CREATE TABLE workflow_instance_snapshots (
  instance_id TEXT PRIMARY KEY,
  workflow_json TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(instance_id) REFERENCES workflow_instances(id) ON DELETE CASCADE
);
CREATE TABLE step_runs (
  id TEXT PRIMARY KEY,
  workflow_instance_id TEXT NOT NULL,
  step_id TEXT NOT NULL,
  agent_id TEXT,
  state TEXT NOT NULL,           -- pending|running|passed|failed|skipped|skipped_cached
  output TEXT,
  structured_output TEXT,        -- JSON-encoded structured output
  summary TEXT,
  exit_code INTEGER,
  skipped_cached BOOLEAN DEFAULT 0,
  started_at TIMESTAMP,
  finished_at TIMESTAMP, publish_payload TEXT, publish_state TEXT, spawned_task_id TEXT REFERENCES internal_tasks(id), input_prompt TEXT, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, total_tokens INTEGER DEFAULT 0, cache_creation_tokens INTEGER DEFAULT 0, cache_read_tokens INTEGER DEFAULT 0, num_turns INTEGER DEFAULT 0, num_tool_calls INTEGER DEFAULT 0, cost_usd REAL DEFAULT 0.0, time_thinking_ms INTEGER NOT NULL DEFAULT 0, time_writing_ms INTEGER NOT NULL DEFAULT 0, time_model_ms INTEGER NOT NULL DEFAULT 0, time_tool_wait_ms INTEGER NOT NULL DEFAULT 0, time_other_ms INTEGER NOT NULL DEFAULT 0, time_background_ms INTEGER NOT NULL DEFAULT 0, slow_tools TEXT,
  FOREIGN KEY(workflow_instance_id) REFERENCES workflow_instances(id)
);
CREATE TABLE ci_poll_checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workflow_instance_id TEXT NOT NULL,
  step_id TEXT NOT NULL,
  status TEXT NOT NULL,           -- passed|failed|pending|timeout|error|unknown
  pr_url TEXT,
  detail TEXT,                    -- JSON of per-check states, or an error message
  checked_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(workflow_instance_id) REFERENCES workflow_instances(id)
);
CREATE TABLE internal_tasks (
  id TEXT PRIMARY KEY,                          -- ulid
  parent_task_id TEXT,                          -- set for spawned tasks (lineage)
  title TEXT NOT NULL,
  description TEXT,
  input TEXT,                                   -- JSON: structured input from spawner
  state TEXT NOT NULL DEFAULT 'registered',     -- registered|running|approval_waiting|done|failed
  metadata TEXT,                                -- JSON: labels, priority, type, etc.
  outstanding_workflows INTEGER DEFAULT 0,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, dedup_key TEXT, generation INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(parent_task_id) REFERENCES internal_tasks(id)
);
CREATE TABLE source_bindings (
  id TEXT PRIMARY KEY,                          -- ulid
  task_id TEXT NOT NULL,
  source_id TEXT NOT NULL,                      -- e.g. "github", "plane"
  source_item_id TEXT NOT NULL,                 -- source-native item ID
  source_item_url TEXT,                         -- deep-link for display
  source_item_number TEXT,                      -- human ref: "#42", "ERP-42"
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(task_id) REFERENCES internal_tasks(id),
  UNIQUE(source_id, source_item_id)
);
CREATE TABLE task_pull_requests (
  id TEXT PRIMARY KEY,                          -- ulid
  task_id TEXT NOT NULL,
  source_id TEXT NOT NULL,                      -- e.g. "github"
  pr_number INTEGER NOT NULL,
  pr_url TEXT NOT NULL,                         -- browser deep-link
  pr_state TEXT,                                -- open|closed|merged, nullable
  seq INTEGER NOT NULL DEFAULT 0,               -- source order; MAX(seq) = most recent
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY(task_id) REFERENCES internal_tasks(id),
  UNIQUE(task_id, source_id, pr_number)
);
CREATE TABLE pr_event_watermarks (
  source_id TEXT PRIMARY KEY,
  watermark TIMESTAMP NOT NULL
);
CREATE TABLE pr_event_dispatches (
  source_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  pr_number INTEGER NOT NULL DEFAULT 0,
  dispatched_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (source_id, event_id, workflow_id)
);
CREATE INDEX idx_pr_event_dispatches_pr ON pr_event_dispatches(source_id, workflow_id, pr_number);
CREATE INDEX idx_executions_task ON task_executions(task_id);
CREATE INDEX idx_executions_retry ON task_executions(next_retry_at) WHERE status='failed';
CREATE INDEX idx_tasks_agent ON tasks(agent_id);
CREATE INDEX idx_tasks_state ON tasks(state);
CREATE INDEX idx_tasks_created ON tasks(created_at DESC);
CREATE INDEX idx_task_logs_task ON task_logs(task_id);
CREATE INDEX idx_task_logs_timestamp ON task_logs(timestamp);
CREATE INDEX idx_service_logs_timestamp ON service_logs(timestamp DESC);
CREATE INDEX idx_execution_events_task ON execution_events(task_id, id);
CREATE INDEX idx_execution_events_instance ON execution_events(workflow_instance_id, id);
CREATE INDEX idx_execution_events_type ON execution_events(type, id);
CREATE TABLE dispatch_jobs (
  id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  project_id TEXT, source_id TEXT, task_id TEXT, workflow_id TEXT,
  agent_id TEXT, runner_id TEXT, pool TEXT,
  required_labels TEXT NOT NULL DEFAULT '[]',
  required_capabilities TEXT NOT NULL DEFAULT '[]',
  affinity_key TEXT, affinity_worker_id TEXT,
  payload_version INTEGER NOT NULL,
  payload TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'queued',
  attempt_count INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  available_at TIMESTAMP NOT NULL,
  lease_attempt_id TEXT, lease_token TEXT, lease_worker_id TEXT,
  lease_expires_at TIMESTAMP,
  terminal_at TIMESTAMP,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
CREATE TABLE dispatch_attempts (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  attempt_number INTEGER NOT NULL,
  worker_id TEXT NOT NULL,
  claim_token TEXT NOT NULL,
  state TEXT NOT NULL,
  lease_expires_at TIMESTAMP NOT NULL,
  heartbeat_at TIMESTAMP NOT NULL,
  started_at TIMESTAMP NOT NULL,
  finished_at TIMESTAMP,
  error_message TEXT,
  FOREIGN KEY(job_id) REFERENCES dispatch_jobs(id),
  UNIQUE(job_id, attempt_number)
);
CREATE TABLE worker_registrations (
  id TEXT PRIMARY KEY,
  protocol_version INTEGER NOT NULL,
  pool TEXT,
  labels TEXT NOT NULL DEFAULT '[]',
  capabilities TEXT NOT NULL DEFAULT '[]',
  capacity INTEGER NOT NULL DEFAULT 1,
  ready INTEGER NOT NULL DEFAULT 1,
  draining INTEGER NOT NULL DEFAULT 0,
  active_jobs INTEGER NOT NULL DEFAULT 0,
  last_heartbeat TIMESTAMP NOT NULL,
  registered_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_dispatch_jobs_claim ON dispatch_jobs(state, available_at, priority DESC, created_at);
CREATE INDEX idx_dispatch_jobs_lease ON dispatch_jobs(state, lease_expires_at);
CREATE INDEX idx_dispatch_jobs_scopes ON dispatch_jobs(state, project_id, source_id, agent_id, runner_id, pool);
CREATE INDEX idx_dispatch_attempts_job ON dispatch_attempts(job_id, attempt_number);
CREATE INDEX idx_dispatch_attempts_lease ON dispatch_attempts(state, lease_expires_at);
CREATE INDEX idx_workers_heartbeat ON worker_registrations(last_heartbeat);
CREATE TABLE approval_requests (
  id TEXT PRIMARY KEY,
  workflow_instance_id TEXT NOT NULL,
  task_id TEXT, workflow_id TEXT, step_id TEXT NOT NULL, message TEXT,
  approvers TEXT NOT NULL DEFAULT '[]', delegates TEXT NOT NULL DEFAULT '{}', required_approvals INTEGER NOT NULL DEFAULT 1, fields TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'pending', response_values TEXT NOT NULL DEFAULT '{}',
  feedback TEXT, responded_by TEXT, response_channel TEXT,
  idempotency_key TEXT UNIQUE, created_at DATETIME NOT NULL, expires_at DATETIME,
  reminded_at DATETIME, escalated_at DATETIME, responded_at DATETIME,
  UNIQUE(workflow_instance_id, step_id)
);
CREATE TABLE approval_responses (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL, decision TEXT NOT NULL, actor TEXT NOT NULL, approver TEXT NOT NULL,
  channel TEXT NOT NULL, idempotency_key TEXT NOT NULL UNIQUE,
  feedback TEXT, values_json TEXT NOT NULL DEFAULT '{}', created_at DATETIME NOT NULL,
  UNIQUE(request_id, approver), FOREIGN KEY(request_id) REFERENCES approval_requests(id)
);
CREATE INDEX idx_approval_requests_status ON approval_requests(status, created_at);
CREATE INDEX idx_approval_requests_instance ON approval_requests(workflow_instance_id);
CREATE INDEX idx_wf_instances_state ON workflow_instances(state);
CREATE INDEX idx_wf_instances_cell ON workflow_instances(cell_id);
CREATE INDEX idx_wf_instances_parent ON workflow_instances(parent_instance_id);
CREATE INDEX idx_step_runs_instance ON step_runs(workflow_instance_id);
CREATE INDEX idx_ci_poll_checks_instance ON ci_poll_checks(workflow_instance_id, step_id);
CREATE INDEX idx_internal_tasks_state ON internal_tasks(state);
CREATE INDEX idx_internal_tasks_parent ON internal_tasks(parent_task_id);
CREATE INDEX idx_source_bindings_task ON source_bindings(task_id);
CREATE INDEX idx_source_bindings_item ON source_bindings(source_id, source_item_id);
CREATE INDEX idx_task_pull_requests_task ON task_pull_requests(task_id);
CREATE INDEX idx_wf_instances_task ON workflow_instances(task_id);
CREATE UNIQUE INDEX idx_internal_tasks_dedup ON internal_tasks(parent_task_id, dedup_key) WHERE dedup_key IS NOT NULL AND dedup_key != '';
CREATE TABLE improvement_runs (
	  id TEXT PRIMARY KEY,
	  effort TEXT NOT NULL,
	  focus TEXT,
	  window_start TIMESTAMP,
	  window_end TIMESTAMP,
	  scope TEXT,                       -- JSON: workflow/agent filters
	  evidence_digest TEXT,             -- hash of the evidence pack, for reproducibility
	  advisor_agent TEXT,
	  advisor_runner TEXT,
	  advisor_model TEXT,
	  report_path TEXT,
	  applied BOOLEAN NOT NULL DEFAULT 0,
	  applied_at TIMESTAMP,
	  cost_usd REAL NOT NULL DEFAULT 0,
	  total_tokens INTEGER NOT NULL DEFAULT 0,
	  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
CREATE TABLE improvement_findings (
	  id TEXT PRIMARY KEY,
	  run_id TEXT NOT NULL,
	  finding_id TEXT,                  -- the advisor's own id, e.g. "f1"
	  scope TEXT NOT NULL,              -- "workflow:x/step:y" | "agent:z"
	  focus TEXT,
	  severity TEXT,
	  confidence TEXT,
	  symptom TEXT,
	  rationale TEXT,
	  target_file TEXT,
	  baseline_metrics TEXT,            -- JSON snapshot of the metrics that justified it
	  patch TEXT,
	  machine_checked BOOLEAN NOT NULL DEFAULT 0,
	  state TEXT NOT NULL,              -- proposed|applied|rejected|reverted
	  reject_reason TEXT,
	  FOREIGN KEY(run_id) REFERENCES improvement_runs(id)
	);
CREATE INDEX idx_improvement_findings_run ON improvement_findings(run_id);
CREATE INDEX idx_improvement_findings_scope ON improvement_findings(scope, state);
CREATE INDEX idx_improvement_runs_created ON improvement_runs(created_at DESC);
