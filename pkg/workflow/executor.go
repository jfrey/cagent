package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	graphagent "github.com/docker/cagent-graph/pkg/agent"
	"github.com/docker/cagent-graph/pkg/engine"
	graphpb "github.com/docker/cagent-graph/pkg/graph/v1"
	"github.com/docker/cagent-graph/pkg/runner"
	"github.com/docker/cagent/pkg/runtime"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/team"
)

// Result holds the outcome of a workflow execution.
type Result struct {
	StepsRun   int
	TotalCost  float64
	StepErrors map[string]error
	Outputs    map[string][]byte // Terminal node content keyed by type.
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

// Executor runs .graph workflows using cagent's agent system.
type Executor struct {
	team    *team.Team
	agentFn graphagent.AgentFunc
	logger  *slog.Logger
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

// RunFile compiles and executes a .graph workflow file. Inputs are seeded
// into the graph before execution; map keys are node types and values are
// content strings.
func (e *Executor) RunFile(ctx context.Context, graphFile string, inputs map[string]string) (*Result, error) {
	r, cleanup, err := e.newRunner()
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	defer cleanup()

	if err := r.CompileFile(graphFile); err != nil {
		return nil, fmt.Errorf("compile %s: %w", graphFile, err)
	}

	return e.run(ctx, r, inputs)
}

// RunSource compiles and executes .graph source bytes.
func (e *Executor) RunSource(ctx context.Context, src []byte, inputs map[string]string) (*Result, error) {
	r, cleanup, err := e.newRunner()
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	defer cleanup()

	if err := r.Compile(src); err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}

	return e.run(ctx, r, inputs)
}

func (e *Executor) newRunner() (*runner.Runner, func(), error) {
	dir, err := os.MkdirTemp("", "cagent-workflow-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp dir: %w", err)
	}
	dbPath := filepath.Join(dir, "graph.db")
	r, err := runner.New(dbPath, e.agentFn)
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	if e.logger != nil {
		r.SetLogger(e.logger)
	}
	cleanup := func() {
		r.Close()
		os.RemoveAll(dir)
	}
	return r, cleanup, nil
}

func (e *Executor) run(ctx context.Context, r *runner.Runner, inputs map[string]string) (*Result, error) {
	workflows := r.Workflows()
	if len(workflows) == 0 {
		return nil, fmt.Errorf("no workflows defined")
	}

	// Seed inputs into the graph.
	g := r.Graph()
	for nodeType, content := range inputs {
		if _, err := g.Put(ctx, &graphpb.PutRequest{
			Nodes: []*graphpb.Node{{
				Type:      nodeType,
				Namespace: workflows[0].Name,
				Content:   []byte(content),
				MediaType: "text/plain",
			}},
		}); err != nil {
			return nil, fmt.Errorf("seed input %s: %w", nodeType, err)
		}
	}

	res, err := r.Run(ctx, 0)
	if err != nil {
		return nil, err
	}

	return &Result{
		StepsRun:   res.StepsRun,
		TotalCost:  res.TotalCost,
		StepErrors: res.StepErrors,
		Outputs:    res.Outputs,
	}, nil
}

// defaultAgentFunc bridges cagent-graph's AgentFunc to cagent's runtime.
// Each invocation creates its own LocalRuntime for safe concurrent execution.
//
// The graph step agent name is matched against the team's named agents. If a
// matching agent exists, its model and configuration are used; otherwise the
// team's default agent is used as a fallback. The graph engine provides the
// full prompt (instruction + graph data) via params.Prompt.
func (e *Executor) defaultAgentFunc(ctx context.Context, params graphagent.AgentParams) (*graphagent.AgentResult, error) {
	// Try to find a matching named agent; fall back to the default.
	a, err := e.team.Agent(params.Agent)
	if err != nil {
		a, err = e.team.DefaultAgent()
		if err != nil {
			return nil, fmt.Errorf("no default agent: %w", err)
		}
	}

	rt, err := runtime.NewLocalRuntime(e.team, runtime.WithCurrentAgent(a.Name()))
	if err != nil {
		return nil, fmt.Errorf("create runtime for step %s: %w", params.Agent, err)
	}

	sess := session.New(
		session.WithSystemMessage(params.Prompt),
		session.WithImplicitUserMessage("Please proceed."),
		session.WithMaxIterations(a.MaxIterations()),
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

// Ensure Result and engine.ExecutionResult stay in sync at compile time.
var _ = func(r *engine.ExecutionResult) *Result {
	return &Result{
		StepsRun:   r.StepsRun,
		TotalCost:  r.TotalCost,
		StepErrors: r.StepErrors,
		Outputs:    r.Outputs,
	}
}
