# Workflow Resume Failure - Problem Statement

**Date:** 2026-02-17
**Severity:** Medium
**Component:** `cagent workflow resume`

---

## Problem Summary

The `workflow resume` command fails to detect and resume workflows that have failed mid-execution. Workflows with partial completion (some steps succeeded, later steps failed) are not identified as resumable.

---

## Expected Behavior

**Scenario:** 3-step workflow where step 2 fails due to timeout

1. ✅ Run workflow with short timeout (step 2 fails)
2. ✅ Step 1 completes successfully, output saved to database
3. ✅ Step 2 fails with timeout error
4. ✅ Update workflow file with longer timeout
5. ✅ Resume workflow with `--allow-changes` flag
6. ✅ Resume skips step 1 (already completed)
7. ✅ Resume re-executes step 2 (failed previously)
8. ✅ Resume continues to step 3
9. ✅ Workflow completes successfully

---

## Actual Behavior

**Resume fails to detect the workflow as resumable:**

```bash
$ cagent workflow resume /tmp/resume.db --workflow /tmp/resume-test.cagent --allow-changes

Error: no resumable workflows found in /tmp/resume.db
```

**Or with explicit namespace:**

```bash
$ cagent workflow resume /tmp/resume.db --namespace multi_step-6c4d06b5... --workflow /tmp/resume-test.cagent --allow-changes

Error: cannot resume: workflow "multi_step-6c4d06b5-c11d-4bfd-93ec" not found in compiled workflows
```

---

## Test Case to Reproduce

### 1. Create Test Workflow

```bash
cat > /tmp/resume-test.cagent << 'EOF'
agent step1 {
  model "anthropic/claude-haiku-4-5"
  skills true
  instruction "You are step 1. Output: Step 1 complete"
}

agent step2 {
  model "anthropic/claude-haiku-4-5"
  skills true
  instruction "You are step 2. Output: Step 2 complete"
}

agent step3 {
  model "anthropic/claude-haiku-4-5"
  skills true
  instruction "You are step 3. Output: Step 3 complete"
}

workflow multi_step {
  graph {
    type input { content text/plain }
    type output1 { content text/plain }
    type output2 { content text/plain }
    type final { content text/plain }
  }

  seed input from inputs

  step step1 {
    reads [input]
    writes [output1]
  }

  step step2 {
    reads [output1]
    writes [output2]
    timeout 1s        # Short timeout - will fail
  }

  step step3 {
    reads [output2]
    writes [final]
  }
}
EOF
```

### 2. Run with Short Timeout (Will Fail)

```bash
$ cagent workflow run /tmp/resume-test.cagent --db /tmp/resume.db --input input="test"

# Output:
time=... level=INFO msg=step.complete agent=step1 step_idx=0 duration=2.97s cost_usd=0.00906
time=... level=ERROR msg=step.error agent=step2 step_idx=1 error="context deadline exceeded"
workflow failed: step step2: error receiving from stream: context deadline exceeded
```

### 3. Verify Partial State

```bash
$ cagent workflow status /tmp/resume.db

Total Nodes: 2
Nodes by Type:
  • input: 1       # ✅ Seeded
  • output1: 1     # ✅ Step 1 completed
  # ❌ output2: missing (step 2 failed)
  # ❌ final: missing (step 3 not started)

$ cagent workflow list /tmp/resume.db

Namespace: multi_step-6c4d06b5-c11d-4bfd-93ec-2a8685e2efb3
  Workflow: multi_step
  Status: failed        # Marked as failed
  Error: step step2: error receiving from stream: context deadline exceeded
```

### 4. Update Timeout and Attempt Resume

```bash
# Edit file: change timeout from 1s to 30s
sed -i '' 's/timeout 1s/timeout 30s/' /tmp/resume-test.cagent

# Attempt resume
$ cagent workflow resume /tmp/resume.db --workflow /tmp/resume-test.cagent --allow-changes

Error: no resumable workflows found in /tmp/resume.db
```

---

## Evidence

### Database State

```sql
sqlite3 /tmp/resume.db "SELECT * FROM workflow_runs;"

6c4d06b5-c11d-4bfd-93ec-2a8685e2efb3|multi_step|6c4d06b5-c11d-4bfd-93ec-2a8685e2efb3|hash123|{}|failed|2026-02-18T00:55:33Z|2026-02-18T00:55:37Z|step step2: error receiving from stream: context deadline exceeded
```

**Observations:**
- Status: `failed`
- Error recorded
- CompletedAt timestamp set (workflow marked as finished)

### Nodes in Database

```sql
sqlite3 /tmp/resume.db "SELECT type, namespace, length(content) FROM nodes;"

input|multi_step-6c4d06b5...|4
output1|multi_step-6c4d06b5...|225
```

**Observations:**
- Step 1 output exists (resumable from step 2)
- Step 2 output missing (needs to be re-run)
- Step 3 output missing (needs to be run)

---

## Possible Root Causes

### 1. Resume Only Checks "running" Status

The `ListWorkflowRuns()` or `GetWorkflowState()` might filter by status:

```go
// Possibly in runner/resume.go
func (r *Runner) ListWorkflowRuns(ctx context.Context) ([]*WorkflowState, error) {
    // BUG: Only returns workflows with status="running"?
    runs, err := r.store.ListWorkflowRuns(ctx, WorkflowRunStatusRunning)
    // Should also check failed workflows with partial completion
}
```

### 2. Workflow Marked as "Completed" When It Fails

If a workflow fails, it might be marked with `CompletedAt` timestamp, making resume logic think it's finished:

```go
// In runner.Run()
defer func() {
    if runErr != nil {
        tracker.FailWorkflowRun(ctx, namespace, runErr.Error())
        // BUG: Sets CompletedAt even though not all steps completed?
    }
}
```

### 3. Namespace Mismatch in Resume

The error `workflow "multi_step-6c4d06b5-c11d-4bfd-93ec" not found` suggests the namespace might be truncated or doesn't match compiled workflow names.

```go
// Namespace in DB: multi_step-6c4d06b5-c11d-4bfd-93ec-2a8685e2efb3
// Workflow name:    multi_step
// Mismatch?
```

### 4. CanResume Flag Not Set Correctly

The `WorkflowState.CanResume` field might be false for failed workflows:

```go
// In GetWorkflowState()
state.CanResume = (status == "running" && hasPartialCompletion)
// Should be: (status == "failed" || status == "running") && hasIncompleteSteps
```

---

## Investigation Steps

1. **Check resume detection logic:**
   - Does `ListWorkflowRuns()` return failed workflows?
   - Does `GetWorkflowState()` mark failed workflows as resumable?

2. **Check step completion detection:**
   - How does resume determine which steps completed?
   - Does it check for output node existence?

3. **Check namespace matching:**
   - Why does namespace lookup fail?
   - Is there a truncation issue?

4. **Check status field logic:**
   - When is a workflow marked "running" vs "failed"?
   - Should failed workflows with partial completion be resumable?

---

## Desired Fix

Resume should work for **failed workflows with partial completion:**

```go
// Workflow is resumable if:
// 1. Status is "failed" OR "running", AND
// 2. At least one step has incomplete outputs, AND
// 3. At least one step completed successfully (has outputs)

canResume := (status == "failed" || status == "running") &&
             len(incompleteSteps) > 0 &&
             len(completedSteps) > 0
```

**Expected resume behavior:**
```bash
$ cagent workflow resume /tmp/resume.db --workflow /tmp/resume-test.cagent --allow-changes

Resuming workflow: multi_step
  Namespace: multi_step-6c4d06b5-c11d-4bfd-93ec-2a8685e2efb3
  Completed: 1/3 steps
  ⚠ Warning: Workflow definition has changed since this run started

Step 2/3 (step2): Running...
Step 2/3 (step2): Complete
Step 3/3 (step3): Running...
Step 3/3 (step3): Complete

✓ Workflow resumed successfully
  Steps executed: 2
  Cost: $0.02
```

---

## Workaround

None currently - resume doesn't work for failed workflows.

Users must:
1. Delete the failed run: `cagent workflow prune --namespace <ns>`
2. Re-run entire workflow from scratch

---

## Related Files

- `pkg/workflow/executor.go` - RunFileWithSelection, runWorkflow
- `cmd/root/workflow.go` - resume command implementation
- `cagent-graph/pkg/runner/resume.go` - GetWorkflowState, ListWorkflowRuns
- Database: `/tmp/resume.db` (test case preserved)
