# plan-build-test workflow

```mermaid
graph TD
    input([feature_request])

    input --> assessor
    assessor --> assessment[(assessment)]
    assessment --> planner
    planner --> plan[(plan)]

    subgraph review_loop ["Review Loop (max 3)"]
        direction TB

        subgraph dev_test ["Dev / Test Loop (max 5)"]
            developer_build[developer] --> code[(code)]
            code --> tester
            tester --> test_result[(test_result)]
            test_result -- "passed == false" --> developer_fix[developer]
            developer_fix -.-> tester
        end

        test_result -- "passed == true" --> reviewer
        assessment -.-> reviewer
        reviewer --> review[(review)]
        review -- "requirements_met == false" --> developer_build
    end

    plan --> developer_build
    review -- "requirements_met == true" --> done([done])

    style input fill:#1565c0,color:#fff
    style done fill:#2e7d32,color:#fff
    style assessment fill:#37474f,color:#fff
    style plan fill:#37474f,color:#fff
    style code fill:#37474f,color:#fff
    style test_result fill:#37474f,color:#fff
    style review fill:#37474f,color:#fff
    style dev_test fill:#4e342e,stroke:#ff9800,color:#fff
    style review_loop fill:#4a148c,stroke:#ce93d8,color:#fff
```

## Agents

| Agent | Tools | Role |
|-------|-------|------|
| assessor | -- | Extract testable requirements from feature request |
| planner | filesystem | Create implementation plan |
| developer | shell, filesystem | Build, fix test failures, fix review feedback |
| tester | shell | Run tests, report pass/fail |
| reviewer | filesystem | Verify requirements are met |

## Data Flow

| Type | Format | Metadata |
|------|--------|----------|
| feature_request | text/plain | -- |
| assessment | text/markdown | -- |
| plan | text/markdown | -- |
| code | text/markdown | -- |
| test_result | text/plain | `passed: bool` |
| review | text/markdown | `requirements_met: bool` |
