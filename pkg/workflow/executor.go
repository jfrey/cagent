package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"encoding/json"
	"strings"

	"github.com/google/uuid"

	graphagent "github.com/docker/cagent-graph/pkg/agent"
	"github.com/docker/cagent-graph/pkg/engine"
	graphpb "github.com/docker/cagent-graph/pkg/graph/v1"
	"github.com/docker/cagent-graph/pkg/runner"
	cagent "github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/environment"
	provider "github.com/docker/cagent/pkg/model/provider"
	"github.com/docker/cagent/pkg/runtime"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/team"
	"github.com/docker/cagent/pkg/teamloader"
	"github.com/docker/cagent/pkg/tools"
)

// Result holds the outcome of a workflow execution.
type Result struct {
	StepsRun   int
	TotalCost  float64
	StepErrors map[string]error
	Outputs    map[string][]byte   // Terminal node content keyed by type.
	OutputIDs  map[string][]string // Terminal node IDs keyed by type.
}

// Option configures an Executor.
type Option func(*Executor)

// WithAgentFunc overrides the default agent callback. Useful for testing.
func WithAgentFunc(fn graphagent.AgentFunc) Option {
	return func(e *Executor) {
		e.agentFn = fn
	}
}

// WithLogger enables step lifecycle logging during workflow execution.
func WithLogger(l *slog.Logger) Option {
	return func(e *Executor) {
		e.logger = l
	}
}

// WithDBPath sets a persistent SQLite database path for the graph store.
// When set, the database is not deleted after execution, enabling inspection
// and resumption of workflows.
func WithDBPath(path string) Option {
	return func(e *Executor) {
		e.dbPath = path
	}
}

// WithRunConfig provides runtime configuration for toolset creation.
// Required for workflows that use inline agent definitions with toolsets.
func WithRunConfig(cfg *config.RuntimeConfig) Option {
	return func(e *Executor) {
		e.runConfig = cfg
	}
}

// Executor runs .cagent workflows using cagent's agent system.
type Executor struct {
	team      *team.Team
	agentFn   graphagent.AgentFunc
	logger    *slog.Logger
	dbPath    string // when set, graph DB persists at this path
	runConfig *config.RuntimeConfig
}

// New creates a workflow Executor backed by the given agent team.
func New(t *team.Team, opts ...Option) *Executor {
	e := &Executor{team: t}
	for _, opt := range opts {
		opt(e)
	}
	if e.agentFn == nil {
		e.agentFn = e.defaultAgentFunc
	}
	return e
}

// RunFile compiles and executes a .cagent workflow file. Inputs are seeded
// into the graph before execution; map keys are node types and values are
// content strings. If multiple workflows are defined (via imports), runs the
// last workflow (typically the composed main workflow).
func (e *Executor) RunFile(ctx context.Context, graphFile string, inputs map[string]string) (*Result, error) {
	return e.RunFileWithSelection(ctx, graphFile, "", inputs)
}

// RunFileWithSelection compiles and executes a specific workflow from a .cagent file.
// If workflowName is empty, auto-detects the root workflow (for composed imports).
func (e *Executor) RunFileWithSelection(ctx context.Context, graphFile string, workflowName string, inputs map[string]string) (*Result, error) {
	r, cleanup, err := e.newRunner()
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	defer cleanup()

	if err := r.CompileFile(graphFile); err != nil {
		return nil, fmt.Errorf("compile %s: %w", graphFile, err)
	}

	// Select which workflow to run using runner's detection
	var workflowIdx int
	workflows := r.Workflows()

	if workflowName != "" {
		// Find by explicit name
		workflowIdx = r.FindWorkflowByName(workflowName)
		if workflowIdx == -1 {
			names := engine.GetWorkflowNames(workflows)
			return nil, fmt.Errorf("workflow %q not found. Available: %s", workflowName, strings.Join(names, ", "))
		}
	} else {
		// Auto-detect root workflow
		workflowIdx = r.FindRootWorkflow()
		if workflowIdx == -1 {
			return nil, fmt.Errorf("no workflows found")
		}
	}

	// Debug: log which workflow was selected
	if e.logger != nil {
		e.logger.Info("Workflow selected",
			"index", workflowIdx,
			"name", workflows[workflowIdx].Name,
			"steps", len(workflows[workflowIdx].Steps),
			"total_workflows", len(workflows))
	}

	return e.runWorkflow(ctx, r, workflowIdx, inputs)
}

// RunSource compiles and executes .cagent source bytes.
// Runs the last workflow if multiple are defined.
func (e *Executor) RunSource(ctx context.Context, src []byte, inputs map[string]string) (*Result, error) {
	r, cleanup, err := e.newRunner()
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	defer cleanup()

	if err := r.Compile(src); err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}

	workflows := r.Workflows()
	workflowIdx := len(workflows) - 1 // Default to last workflow
	if workflowIdx < 0 {
		return nil, fmt.Errorf("no workflows defined")
	}

	return e.runWorkflow(ctx, r, workflowIdx, inputs)
}

func (e *Executor) newRunner() (*runner.Runner, func(), error) {
	var dbPath string
	var tempDir string

	if e.dbPath != "" {
		// Persistent DB — user specified a path.
		dbPath = e.dbPath
	} else {
		// Ephemeral DB — create a temp dir that gets cleaned up.
		dir, err := os.MkdirTemp("", "cagent-workflow-*")
		if err != nil {
			return nil, nil, fmt.Errorf("create temp dir: %w", err)
		}
		tempDir = dir
		dbPath = filepath.Join(dir, "graph.db")
	}

	r, err := runner.New(dbPath, e.agentFn)
	if err != nil {
		if tempDir != "" {
			os.RemoveAll(tempDir)
		}
		return nil, nil, err
	}
	if e.logger != nil {
		r.SetLogger(e.logger)
	}
	cleanup := func() {
		r.Close()
		if tempDir != "" {
			os.RemoveAll(tempDir)
		}
	}
	return r, cleanup, nil
}

func (e *Executor) runWorkflow(ctx context.Context, r *runner.Runner, workflowIdx int, inputs map[string]string) (*Result, error) {
	workflows := r.Workflows()
	if len(workflows) == 0 {
		return nil, fmt.Errorf("no workflows defined")
	}
	if workflowIdx < 0 || workflowIdx >= len(workflows) {
		return nil, fmt.Errorf("workflow index %d out of range [0, %d)", workflowIdx, len(workflows))
	}

	// Generate a run ID so we can seed into the correct namespace.
	// The runner creates namespace = workflowName + "-" + runID.
	runID := uuid.New().String()
	namespace := workflows[workflowIdx].Name + "-" + runID

	// Seed inputs into the graph.
	g := r.Graph()
	for nodeType, content := range inputs {
		if _, err := g.Put(ctx, &graphpb.PutRequest{
			Nodes: []*graphpb.Node{{
				Type:      nodeType,
				Namespace: namespace,
				Content:   []byte(content),
				MediaType: "text/plain",
			}},
		}); err != nil {
			return nil, fmt.Errorf("seed input %s: %w", nodeType, err)
		}
	}

	res, err := r.Run(ctx, workflowIdx, runner.WithRunID(runID))
	if err != nil {
		return nil, err
	}

	return &Result{
		StepsRun:   res.StepsRun,
		TotalCost:  res.TotalCost,
		StepErrors: res.StepErrors,
		Outputs:    res.Outputs,
		OutputIDs:  res.OutputIDs,
	}, nil
}

// defaultAgentFunc bridges cagent-graph's AgentFunc to cagent's runtime.
// Each invocation creates its own LocalRuntime for safe concurrent execution.
//
// When a team is provided (--agents), the graph step name is matched against
// the team's named agents. When no team exists, agents are built on-the-fly
// from the .cagent file's agent/model/provider definitions in AgentParams.
func (e *Executor) defaultAgentFunc(ctx context.Context, params graphagent.AgentParams) (*graphagent.AgentResult, error) {
	// Convert graph MCP tools to cagent tools with handlers that route
	// back to the graph via params.ToolCall.
	var graphTools []tools.Tool
	if params.ToolCall != nil {
		for _, mt := range params.MCPTools {
			graphTools = append(graphTools, mcpToolToCagentTool(mt, params.ToolCall))
		}
	}

	// Build the runtime and resolve agent config.
	rt, instruction, maxIter, err := e.buildRuntime(ctx, params, graphTools)
	if err != nil {
		return nil, err
	}

	// Build the system message from:
	// 1. Workflow execution context (how the pipeline works)
	// 2. Agent instruction (from .cagent or agent.yaml)
	// 3. Prompt data from the graph engine (node content to process)
	var systemParts []string
	systemParts = append(systemParts, workflowContextPrompt)
	if instruction != "" {
		systemParts = append(systemParts, instruction)
	}
	if params.Prompt != "" {
		systemParts = append(systemParts, "--- INPUT DATA ---\n"+params.Prompt)
	}
	systemMsg := strings.Join(systemParts, "\n\n")

	sess := session.New(
		session.WithSystemMessage(systemMsg),
		session.WithImplicitUserMessage("Please proceed."),
		session.WithMaxIterations(maxIter),
		session.WithToolsApproved(true),
		session.WithSendUserMessage(false),
	)

	var totalIn, totalOut int64
	var totalCost float64

	for event := range rt.RunStream(ctx, sess) {
		switch ev := event.(type) {
		case *runtime.ErrorEvent:
			return nil, fmt.Errorf("step %s: %s", params.Agent, ev.Error)
		case *runtime.TokenUsageEvent:
			if ev.Usage != nil {
				totalIn = ev.Usage.InputTokens
				totalOut = ev.Usage.OutputTokens
				totalCost = ev.Usage.Cost
			}
		}
	}

	return &graphagent.AgentResult{
		Output:    sess.GetLastAssistantMessageContent(),
		TokensIn:  int(totalIn),
		TokensOut: int(totalOut),
		CostUSD:   totalCost,
	}, nil
}

// buildRuntime creates a LocalRuntime for a workflow step. When a team exists
// (from --agents), it uses the team's agents. When no team exists, it builds
// an agent on-the-fly from the .cagent file's definitions.
func (e *Executor) buildRuntime(ctx context.Context, params graphagent.AgentParams, graphTools []tools.Tool) (*runtime.LocalRuntime, string, int, error) {
	if e.team != nil {
		return e.buildRuntimeFromTeam(ctx, params, graphTools)
	}
	return e.buildRuntimeFromGraph(ctx, params, graphTools)
}

// buildRuntimeFromTeam creates a runtime using the team's named agents.
func (e *Executor) buildRuntimeFromTeam(ctx context.Context, params graphagent.AgentParams, graphTools []tools.Tool) (*runtime.LocalRuntime, string, int, error) {
	a, err := e.team.Agent(params.Agent)
	if err != nil {
		a, err = e.team.DefaultAgent()
		if err != nil {
			return nil, "", 0, fmt.Errorf("no default agent: %w", err)
		}
	}

	rtOpts := []runtime.Opt{runtime.WithCurrentAgent(a.Name())}
	if len(graphTools) > 0 {
		rtOpts = append(rtOpts, runtime.WithExtraTools(graphTools))
	}

	rt, err := runtime.NewLocalRuntime(e.team, rtOpts...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("create runtime for step %s: %w", params.Agent, err)
	}

	// .cagent instruction is canonical; fall back to team agent.
	instruction := params.Instruction
	if instruction == "" {
		instruction = a.Instruction()
	}

	maxIter := a.MaxIterations()
	if params.AgentDef != nil && params.AgentDef.MaxIterations > 0 {
		maxIter = params.AgentDef.MaxIterations
	}

	return rt, instruction, maxIter, nil
}

// buildRuntimeFromGraph creates a runtime from the .cagent file's agent/model/provider
// definitions — no agent.yaml needed.
func (e *Executor) buildRuntimeFromGraph(ctx context.Context, params graphagent.AgentParams, graphTools []tools.Tool) (*runtime.LocalRuntime, string, int, error) {
	if params.AgentDef == nil {
		return nil, "", 0, fmt.Errorf("step %s: no agent definition (provide --agents or define agents in .cagent)", params.Agent)
	}

	// Resolve the model config from .cagent definitions.
	modelCfg, err := resolveModelConfig(params)
	if err != nil {
		return nil, "", 0, fmt.Errorf("step %s: %w", params.Agent, err)
	}

	// Create a provider from the resolved model config.
	env := environment.NewOsEnvProvider()
	p, err := provider.New(ctx, modelCfg, env)
	if err != nil {
		return nil, "", 0, fmt.Errorf("step %s: create provider: %w", params.Agent, err)
	}

	// Convert toolsets from .cagent to cagent toolsets
	var agentOpts []cagent.Opt
	agentOpts = append(agentOpts,
		cagent.WithModel(p),
		cagent.WithDescription(params.AgentDef.Description),
		cagent.WithMaxIterations(params.AgentDef.MaxIterations),
		cagent.WithSkillsEnabled(params.AgentDef.Skills),
	)

	// Process toolsets if defined
	if len(params.AgentDef.Toolsets) > 0 {
		// Use teamloader to convert toolsets (handles all types and properties)
		workDir := "."
		if e.runConfig != nil && e.runConfig.WorkingDir != "" {
			workDir = e.runConfig.WorkingDir
		}

		// Create minimal runConfig if not provided
		runCfg := e.runConfig
		if runCfg == nil {
			runCfg = &config.RuntimeConfig{
				EnvProviderForTests: environment.NewOsEnvProvider(),
			}
		}

		toolsets, warnings := teamloader.ConvertGraphToolsets(ctx, params.AgentDef.Toolsets, workDir, runCfg)
		if len(warnings) > 0 {
			agentOpts = append(agentOpts, cagent.WithLoadTimeWarnings(warnings))
		}
		if len(toolsets) > 0 {
			agentOpts = append(agentOpts, cagent.WithToolSets(toolsets...))
		}
	}

	// Build agent with toolsets
	a := cagent.New(params.Agent, params.Instruction, agentOpts...)

	// Build a single-agent team.
	t := team.New(team.WithAgents(a))

	rtOpts := []runtime.Opt{runtime.WithCurrentAgent(params.Agent)}
	if len(graphTools) > 0 {
		rtOpts = append(rtOpts, runtime.WithExtraTools(graphTools))
	}

	rt, err := runtime.NewLocalRuntime(t, rtOpts...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("create runtime for step %s: %w", params.Agent, err)
	}

	return rt, params.Instruction, params.AgentDef.MaxIterations, nil
}

// resolveModelConfig converts .cagent model/provider definitions into a
// latest.ModelConfig that cagent's provider system understands.
func resolveModelConfig(params graphagent.AgentParams) (*latest.ModelConfig, error) {
	modelName := params.Model
	if modelName == "" && params.AgentDef != nil {
		modelName = params.AgentDef.Model
	}
	if modelName == "" {
		return nil, fmt.Errorf("no model specified")
	}

	// Check if the model name references a .cagent model definition.
	if md, ok := params.Models[modelName]; ok {
		cfg := &latest.ModelConfig{
			Provider: md.Provider,
			Model:    md.Model,
			BaseURL:  md.BaseURL,
			TokenKey: md.TokenKey,
		}
		if md.MaxTokens > 0 {
			cfg.MaxTokens = &md.MaxTokens
		}
		if md.Temperature != nil {
			cfg.Temperature = md.Temperature
		}
		if md.TopP != nil {
			cfg.TopP = md.TopP
		}
		if md.ContextLimit > 0 {
			cfg.ContextLimit = &md.ContextLimit
		}
		if md.ProviderOpts != nil {
			cfg.ProviderOpts = md.ProviderOpts
		}

		// Merge provider-level config (base_url, token_key) if not set on model.
		if pd, ok := params.Providers[md.Provider]; ok {
			if cfg.BaseURL == "" {
				cfg.BaseURL = pd.BaseURL
			}
			if cfg.TokenKey == "" {
				cfg.TokenKey = pd.TokenKey
			}
		}
		return cfg, nil
	}

	// Try as an inline "provider/model" reference (e.g., "anthropic/claude-haiku-4-5").
	if p, m, ok := strings.Cut(modelName, "/"); ok {
		cfg := &latest.ModelConfig{
			Provider: p,
			Model:    m,
		}
		if pd, ok := params.Providers[p]; ok {
			cfg.BaseURL = pd.BaseURL
			cfg.TokenKey = pd.TokenKey
		}
		return cfg, nil
	}

	return nil, fmt.Errorf("model %q not found in .cagent definitions and not a provider/model reference", modelName)
}

// workflowContextPrompt explains the execution model to agents so they
// understand their role in the pipeline and how their output is used.
const workflowContextPrompt = `You are a step in a workflow pipeline. Here is how it works:

- You receive INPUT DATA below from previous steps in the pipeline.
- Your TEXT RESPONSE is your output. It will be passed as input to downstream steps.
- Write your response directly — your text IS the primary deliverable.
- You may also have graph database tools (graph_put, graph_get, etc.) for storing structured data persistently. Use them when you want to store rich metadata, build relationships between entities, or preserve data that doesn't fit naturally in prose. When you store data in the graph, mention the node IDs and types in your text response so downstream steps know what's available and can retrieve it with graph_get.`

// mcpToolToCagentTool converts a graph MCP tool definition into a cagent Tool
// with a handler that routes calls back through the graph's ToolCallFunc.
func mcpToolToCagentTool(mt graphagent.MCPTool, callFn graphagent.ToolCallFunc) tools.Tool {
	// Parse the input schema into a map for the cagent tool definition.
	var schema any
	if len(mt.InputSchema) > 0 {
		if err := json.Unmarshal(mt.InputSchema, &schema); err != nil {
			slog.Warn("Failed to parse tool input schema", "tool", mt.Name, "error", err)
		}
	}

	return tools.Tool{
		Name:        mt.Name,
		Category:    "graph",
		Description: mt.Description,
		Parameters:  schema,
		Handler: func(ctx context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
			result, err := callFn(ctx, tc.Function.Name, []byte(tc.Function.Arguments))
			if err != nil {
				return tools.ResultError(err.Error()), nil
			}
			return tools.ResultSuccess(result), nil
		},
	}
}

// Ensure Result and engine.ExecutionResult stay in sync at compile time.
var _ = func(r *engine.ExecutionResult) *Result {
	return &Result{
		StepsRun:   r.StepsRun,
		TotalCost:  r.TotalCost,
		StepErrors: r.StepErrors,
		Outputs:    r.Outputs,
		OutputIDs:  r.OutputIDs,
	}
}

// Resume compiles a workflow and resumes execution from a previous run.
func (e *Executor) Resume(ctx context.Context, workflowFile string, namespace string, opts *runner.ResumeOptions) (*Result, error) {
	r, cleanup, err := e.newRunner()
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	defer cleanup()

	// Compile workflow file
	if err := r.CompileFile(workflowFile); err != nil {
		return nil, fmt.Errorf("compile workflow: %w", err)
	}

	// Auto-detect namespace if not specified
	if namespace == "" {
		runs, err := r.ListWorkflowRuns(ctx)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}

		if e.logger != nil {
			e.logger.Info("Checking for resumable workflows", "total_runs", len(runs))
			for i, run := range runs {
				e.logger.Info("Workflow run", "index", i, "namespace", run.Namespace,
					"completed_steps", len(run.CompletedSteps), "total_steps", run.TotalSteps,
					"can_resume", run.CanResume, "reason", run.Reason)
			}
		}

		for _, run := range runs {
			if run.CanResume {
				namespace = run.Namespace
				if e.logger != nil {
					e.logger.Info("Auto-selected resumable workflow", "namespace", namespace)
				}
				break
			}
		}

		if namespace == "" {
			return nil, fmt.Errorf("no resumable workflows found (checked %d runs)", len(runs))
		}
	}

	// Resume execution
	res, err := r.Resume(ctx, namespace, opts)
	if err != nil {
		return nil, err
	}

	return &Result{
		StepsRun:   res.StepsRun,
		TotalCost:  res.TotalCost,
		StepErrors: res.StepErrors,
		Outputs:    res.Outputs,
		OutputIDs:  res.OutputIDs,
	}, nil
}
