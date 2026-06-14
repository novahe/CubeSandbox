# AGENTS Policy



## AI-Generated Code Policy

AI agents MUST NOT add Signed-off-by tags. Only humans can legally certify the Developer Certificate of Origin (DCO). The human submitter is responsible for:

- Reviewing all AI-generated code
- Ensuring compliance with licensing requirements
- Adding their own Signed-off-by tag to certify the DCO
- Taking full responsibility for the contribution

**MUST FOLLOW THIS**: When performing a `git commit` or submitting a GitHub PR, the commit message or PR description MUST include the following tag — this is required so that agent contributions remain visible and attributable in the project history:

- If the work was **human-assisted by an AI agent**, include:

```
Assisted-by: AGENT_NAME:MODEL_VERSION
```

- If the commit/PR was **fully completed autonomously by an AI agent** (without human authoring), include instead:

```
Autonomously-by: AGENT_NAME:MODEL_VERSION
```

Where:
- `AGENT_NAME` is the name of the AI tool or framework
- `MODEL_VERSION` is the specific model version used

## Go Testing Policy

When running Go tests in projects that use `agiledragon/gomonkey` for mocking, you MUST disable compiler inlining to prevent panic or mock failures.

**MUST FOLLOW THIS**: Always include the `-gcflags="all=-l"` flag when running `go test`:

```bash
go test -gcflags="all=-l" -v ./...
```

## Cross-Platform Linux Testing Policy

When running Go tests on macOS (Darwin) that depend on Linux-specific syscalls (e.g., `unix.F_SETPIPE_SZ`, `unix.CAP_SYS_ADMIN`), native `go test` will fail to compile. 

**MUST FOLLOW THIS**: To run these tests or debug them, assume a persistent Linux Docker container named `golang-linux-test` is already running with the workspace mounted. Directly execute tests inside it:

Execute tests inside the container (remember to append `-gcflags="all=-l"` if `gomonkey` is used):
```bash
docker exec golang-linux-test bash -c "cd <your_project_path> && go test -gcflags='all=-l' -v ."
```
