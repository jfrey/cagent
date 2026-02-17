# Lookout Code Review - Workflow Commands

**Review Date:** 2026-02-17
**Total Cost:** $4.8567
**Pipeline:** 23 raw → 16 consolidated → 13 validated → 13 reported
**Severity Breakdown:** 0 critical, 0 high, 7 medium, 6 low

---

## Review Process

```
[setup] Claude CLI: /opt/homebrew/bin/claude
[specialize] Running 4 specialist agents in parallel...
[specialize] Found 23 raw findings (cost: $3.2193)
[consolidate] Consolidating 23 findings...
[consolidate] Consolidated to 16 findings (cost: $0.2232)
[validate] Validating 16 findings...
[validate] 13 of 16 findings validated (cost: $1.4142)
[report] Applying thresholds and building review comments...
[report] 13 findings to report

Agent Execution:
- security-reviewer: $0.9179 (15 turns)
- code-reviewer: $0.7645 (10 turns)
- test-reviewer: $0.7943 (19 turns)
- doc-reviewer: $0.7425 (17 turns)
- consolidator: $0.2232 (3 turns)
- validator: $1.4142 (63 total turns across 4 validators)
```

---

## Findings

### [MEDIUM] pkg/tools/builtin/limit.go:4
**Dangerous 333x increase in maxOutputSize from 30KB to 10MB**

maxOutputSize was increased from 30,000 to 10,000,000 bytes (10MB), a 333x increase. This creates multiple compounding risks:

1. **Memory exhaustion** -- tools can now allocate 10MB per call, and concurrent tool usage can rapidly exhaust memory, enabling DoS.
2. **LLM context window overflow** -- 10MB of tool output will blow through most model context limits (~200K tokens is ~800KB text).
3. **Performance degradation** from copying/processing 10MB strings repeatedly.
4. **Attack surface** -- an attacker who controls workflow inputs or .cgt file contents can craft workflows that generate large tool outputs, causing resource exhaustion.

The old 30KB limit was intentionally conservative to prevent these exact issues.

**Suggestion:** Revert to 30KB or use a more modest increase (e.g., 100KB). If large outputs are genuinely needed for specific tools, implement:
- Per-tool limits with streaming/pagination
- Per-workflow aggregate limits
- Rate limiting based on resource usage
- Document the memory implications for operators

---

### [MEDIUM] cmd/root/workflow.go:438
**Fixed limit of 1000 nodes in workflow status command silently truncates results**

The status command (line 438) queries nodes with a hardcoded limit of 1000. For large workflows, this silently truncates results without warning the user. The status display will show incomplete statistics (node counts, types), misleading users about workflow state.

**Suggestion:** Either:
1. Page through all results using multiple queries with offset/limit
2. Add a warning when results are truncated: `if len(resp.Nodes) >= 1000 { fmt.Fprintf(cmd.ErrOrStderr(), "Warning: showing first 1000 nodes\n") }`
3. Make the limit configurable via flag

---

### [LOW] cmd/root/workflow.go:752
**Inefficient cascading error handling in runWorkflowList**

Lines 752-768 make three separate ListAllWorkflowRuns calls and use a cascading if-else to check errors. If the first call fails, it still attempts the other two calls before reporting the error. This wastes time and resources, and the verbose error checking pattern (err1, err2, err3) is error-prone.

**Suggestion:** Short-circuit on first error: check each call's error immediately before proceeding to the next. This is clearer and fails fast.

---

### [MEDIUM] cmd/root/workflow.go:102
**Workflow command with 5 subcommands (491 lines) has zero test coverage**

The new workflow command with run, lint, describe, init, and status subcommands totaling 491 lines has zero test coverage. Critical user-facing functionality like:
- Input parsing (lines 109-116)
- Agent loading (lines 120-131)
- Error formatting (lines 178-183)

...are completely untested.

**Suggestion:** Add `cmd/root/workflow_test.go` with test cases:
- TestWorkflowRunWithInputs
- TestWorkflowLintInvalidFile
- TestWorkflowInitFileExists
- TestWorkflowStatusNoDatabase

---

### [MEDIUM] pkg/workflow/executor.go:425
**JSON unmarshal error silently ignored in mcpToolToCagentTool, and conversion logic is untested**

Line 425 unmarshals InputSchema but ignores the error: `_ = json.Unmarshal(mt.InputSchema, &schema)`. If the schema is malformed JSON, it silently becomes nil/empty and the tool gets registered with no parameter schema.

This leads to:
1. LLM receiving no parameter constraints for the tool
2. Tool calls failing at runtime with cryptic errors
3. Difficult debugging with no indication the schema failed to parse

Additionally, the entire mcpToolToCagentTool function (lines 421-441) including schema parsing, handler closure, and tool call routing has no test coverage.

**Suggestion:**
- Check and log the unmarshal error: `if err := json.Unmarshal(mt.InputSchema, &schema); err != nil { slog.Warn("Failed to parse tool input schema", "tool", mt.Name, "error", err) }`
- Consider skipping tools with invalid schemas
- Add tests for mcpToolToCagentTool covering:
  - Valid schema unmarshaling
  - Invalid JSON in InputSchema
  - Handler routing

---

### [LOW] pkg/workflow/executor.go:159
**Predictable runID based on timestamp creates collision risk**

The runID is generated using `time.Now().UnixNano()` (line 159). In high-throughput scenarios or testing with mocked time, multiple workflows started in the same nanosecond will have identical runIDs, causing:
- Namespace collisions
- Workflows clobbering each other's graph data
- Incorrect state tracking
- Potential data corruption in the SQLite database

**Suggestion:** Use a proper UUID generator:
```go
runID := uuid.New().String()
```
Or combine timestamp with random data:
```go
runID := fmt.Sprintf("%d-%s", time.Now().UnixNano(), randString(8))
```

---

### [MEDIUM] pkg/workflow/executor.go:197
**Executor runtime building and execution error paths have no test coverage**

The executor's core pipeline has multiple error paths that are completely untested:
- buildRuntime
- buildRuntimeFromTeam
- buildRuntimeFromGraph
- defaultAgentFunc
- resolveModelConfig

This includes:
1. buildRuntimeFromTeam with no matching or default agent
2. buildRuntimeFromGraph with nil AgentDef
3. resolveModelConfig with missing model/provider definitions and precedence edge cases
4. defaultAgentFunc error scenarios (ErrorEvent propagation, nil Usage fields, empty assistant messages)
5. provider.New failures

Only happy paths are tested indirectly.

**Suggestion:** Add targeted unit tests covering:
- Missing agent resolution
- Nil AgentDef handling
- Model config resolution precedence (partial config, provider merging conflicts, unknown inline providers)
- runtime.RunStream ErrorEvent propagation
- TokenUsageEvent with nil Usage

---

### [MEDIUM] pkg/workflow/executor.go:115
**Database persistence and cleanup logic not tested**

The executor supports persistent databases via WithDBPath option (lines 57-61) and has logic to create ephemeral vs persistent databases in newRunner (lines 115-148), but no tests verify that:
- Persistent databases survive after cleanup
- Ephemeral databases are properly deleted
- Database path errors are handled correctly

**Suggestion:** Add tests:
1. TestRunSourceWithPersistentDB verifying the database file exists after execution
2. TestRunSourceWithEphemeralDB verifying temp directory cleanup
3. TestRunSourceWithInvalidDBPath testing error handling

---

### [LOW] pkg/workflow/executor.go:86
**RunFile public API not tested**

The RunFile method (lines 86-98) is a public API that compiles and executes workflow files, but only RunSource is tested. File compilation errors, invalid file paths, and file I/O errors are untested.

**Suggestion:** Add TestRunFile test cases covering:
- Successful file execution with a real .cgt file
- File not found error
- Compilation errors from malformed workflow files

---

### [MEDIUM] pkg/workflow/executor.go:63
**Comments say '.graph' but codebase uses '.cgt' file extension**

Multiple doc comments in executor.go reference '.graph' as the workflow file extension, but throughout the codebase (including user-facing CLI help text) the actual extension is '.cgt'.

Affected locations:
- Line 63 (Executor struct comment)
- Line 83 (RunFile doc comment)
- Line 100 (RunSource doc comment)

This inconsistency confuses developers working on the code.

**Suggestion:** Update all three comments to reference '.cgt' instead of '.graph':
- Line 63: `// Executor runs .cgt workflows using cagent's agent system.`
- Line 83: `// RunFile compiles and executes a .cgt workflow file.`
- Line 100: `// RunSource compiles and executes .cgt source bytes.`

---

### [LOW] pkg/modelsdev/store.go:201
**Proxied model fallback silently changes lookup semantics**

Lines 872-880 add a fallback that parses the modelID as 'provider/model' when the initial lookup fails. This changes GetModel behavior: a lookup for 'openai/anthropic/claude-haiku' will fail to find it under 'openai', then silently re-parse it as 'anthropic/claude-haiku'.

This makes debugging harder, violates principle of least surprise, and could mask configuration errors where the wrong provider is specified.

**Suggestion:** Add logging when falling back to proxied model resolution:
```go
slog.Debug("Model not found in provider, trying as proxied model",
    "original_id", modelID,
    "provider", realProvider,
    "model", realModel)
```
Document this behavior in the function comment.

---

### [LOW] pkg/modelsdev/store.go:213
**Missing doc comment for getModelDirect explaining its role vs GetModel**

The function getModelDirect's doc comment doesn't explain when to use it vs GetModel or why it exists. It explains what it does but not its role in the proxied model fallback flow.

**Suggestion:** Expand the doc comment to clarify:
```go
// getModelDirect looks up a model by explicit provider and model name,
// without going through the full ID parsing logic. Used internally by
// GetModel for proxied model fallback when the initial lookup fails.
```

---

### [LOW] pkg/runtime/runtime.go:259
**New runtime options (WithExtraTools, ContextLimit) lack test coverage**

Two new runtime features lack test coverage:
1. WithExtraTools (lines 259-266, 1036-1039) -- the tool list merging behavior when extra tools are appended to agent tools is not verified
2. ContextLimit override (lines 1020-1028) -- the precedence logic (explicit ContextLimit > models.dev lookup) and behavior with zero/negative values are untested

**Suggestion:** Add tests:
- TestLocalRuntimeWithExtraTools verifying extra tools appear in the tool list
- TestSessionCompactionWithContextLimitOverride verifying compaction triggers correctly with explicit ContextLimit values

---

## Summary

**Files Changed:** 13
**Issues Found:** 13
**Severity Distribution:**
- Critical: 0
- High: 0
- Medium: 7 (54%)
- Low: 6 (46%)

**Top 3 Priorities:**
1. Review and adjust the 10MB output limit (security/performance risk)
2. Add test coverage for workflow commands (491 lines untested)
3. Fix status command 1000-node truncation (silent data loss)

**Review Costs:**
- Specialization: $3.22
- Consolidation: $0.22
- Validation: $1.41
- **Total: $4.86**
