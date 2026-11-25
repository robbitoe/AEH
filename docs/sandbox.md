# Sandbox

Without a sandbox, AEH runs your build, test and acceptance commands on your machine with your user's permissions. Those commands execute code an agent just wrote. With the optional sandbox, they run in a local Docker container instead:

- no network (only loopback),
- no forwarded host credentials: `pass_env` is not forwarded and proxy variables are empty (secrets committed to the repository or built into the image are not removed),
- no Git metadata and no host directory: the container sees an exact copy of the commit,
- a read-only root filesystem, all capabilities dropped, user `10001:10001`, memory and CPU limits.

After every command, AEH reads the copy back and checks it. Tracked files must keep their bytes and modes. New files must be ignored by the commit's `.gitignore`. Links and devices are refused. A command that changes a sealed test, a tracked file or plants a link fails, exactly like "verification changed the checkout" on the host.

With `sandbox.agent_image`, every agent also runs in a container: implementers, reviewers, triage, test and characterization authors, discovery authors, book reviewers and adoption inspectors. See [Agents in containers](#agents-in-containers).

## Requirements

- Docker Engine 24 or later, local, with Linux containers: Docker Desktop on Windows or macOS, or Docker Engine on Linux. Remote endpoints (`tcp://`, `ssh://`) are refused.
- An image you build, holding every tool and dependency your commands need. AEH never pulls or publishes images.
- Build, test and acceptance commands written as POSIX shell commands: they run with `sh -c` inside the container on every host.

## Set it up

1. Build an image for your project. [`docs/sandbox/Dockerfile.tools`](sandbox/Dockerfile.tools) is an example for a Node.js project. Dependencies go outside `/work`, because AEH mounts the commit there.

   ```sh
   docker build -t myproject-gates -f Dockerfile.tools .
   docker image inspect -f '{{.Id}}' myproject-gates
   ```

2. Add the section to `aeh/config.yaml`, with the ID you just printed:

   ```yaml
   sandbox:
     image: "sha256:3f1c..."   # or name@sha256:<digest> for a pushed image
     memory: 4g                # default 4g
     cpus: 2                   # default 2
     gate_env:                 # literal, non-secret variables; HOME and TMPDIR are set by aeh
       DOTNET_CLI_TELEMETRY_OPTOUT: "1"
   ```

3. Check it:

   ```sh
   aeh sandbox check
   ```

   It checks the engine, the image and a throwaway container with the same isolation as a gate. It also warns when a command calls `git`, since the container has no repository.

4. Commit `aeh/config.yaml`.

From then on, `aeh run`, `aeh do`, `aeh plan`, `aeh adopt seal` and `aeh adopt verify` run every build, test and acceptance command in the sandbox. A configured sandbox is never skipped. If Docker is stopped or the image is missing, the command stops before any agent runs, or, when it happens in the middle of a run, the step blocks with the reason without consuming a ladder attempt. Start Docker (or fix the image), then run the command again or answer the question.

## What changes for your project

- **Dependencies belong in the image.** A build that downloads packages fails, because there is no network. Rebuild the image when dependencies change, and update `sandbox.image`.
- **Each command is a new container** on the same copy of the commit. Build output that later commands need (for example a compiled binary used by the tests) survives between build, test and acceptance of the same verification. It must be ignored by `.gitignore`, as on the host.
- **Exact repository bytes.** The copy is made from Git objects, with no checkout conversion: no `core.autocrlf`, no `eol` attributes, no filters. A file committed with LF has LF in the container on every host. Executable bits come from the commit (`100755`), not from your file system.
- **Symbolic links and submodules** in the tracked tree are refused under the sandbox, with the paths named.
- **The acceptance manifest** is a read-only file under `/run/aeh/`; `{{manifest}}` is replaced with its path.
- **Output** keeps its last 64 KiB in memory; reports show the tail, as before.

## Evidence

Each gate in `gates.json` records `"sandboxed": true`, its class (`process`, `infrastructure`, `resource` for an out-of-memory kill, `terminated` for a timeout or cancellation) and its exit code. Logs and reports show `ok (sandbox)`. Only a process that exited by itself proves a red or a green: a test killed for memory never counts as a failing test when a plan checks that new tests fail.

Runs and plans pin the sandbox in their contract: the policy version, the Docker engine ID, the image ID, the limits and `gate_env`. Changing any of them, or turning the sandbox on or off, means a new outcome ID or a new plan: a blocked run with a different sandbox is not resumed, and a planned run starts only under the sandbox its plan's checks ran under. Plan checks record whether they ran sandboxed and how their process ended.

## Agents in containers

Set `sandbox.agent_image` to run every agent in a container too (sandbox policy version 2):

```yaml
sandbox:
  image: "sha256:3f1c..."
  agent_image: "sha256:9b2e..."   # an image with the agent CLIs your profiles run
profiles:
  fuerte:
    provider: anthropic
    command: ["claude", "-p", "--safe-mode", "--permission-mode", "acceptEdits", "--permission-prompts", "none", "--output-format", "text"]
    stdin: true
    sandbox:
      pass_env: [ANTHROPIC_API_KEY]   # host variables this profile receives in its container
```

Each agent launch gets only what its step needs:

| Agent | `/work` | `/out` | `/memory` |
| --- | --- | --- | --- |
| implementer | a copy of the base commit, writable | none | product memory, read-only |
| QA, security and DBA reviewers | a copy of the candidate, read-only | none | product memory, read-only |
| triage | a copy of the base commit, read-only | none | product memory, read-only |
| test or characterization author (`aeh plan`) | a copy of the plan's base, writable | none | the plan's pinned memory, read-only |
| plan reviewers | a copy of the plan commit, read-only | none | the plan's pinned memory, read-only |
| discovery author | none | its output, writable | product memory, read-only |
| book reviewer | the draft book, read-only | its findings, writable | none |
| adoption inventory and area inspectors | a filtered copy of the inspected commit, read-only | its output, writable | none |

- **Nothing an agent writes is checked out on the host.** AEH reads the implementer's `/work` back and records it as the candidate commit from Git objects, through a private index; no `git add`, filter, hook or attribute runs on agent content. New files ignored by the base commit's `.gitignore` are left out. A link, a device, a `.git` entry or an oversized file fails the attempt with the path, like any other agent failure. Reviewers, plan checks and the final check read exports of the commit, so no host worktree holds agent content. A plan with no test to write (only retirements) is still written by aeh on a host checkout of its base.
- **Outputs** written to `/out` are read back into memory and checked exactly like a host output directory: only the expected files, portable names, size limits. Nothing is written to the host's file system. `{{output_dir}}` in a profile's command becomes `/out`, and adoption prompts name `/out`.
- **Adoption inspectors** get the same filtered view as on the host: possible secrets (`.env*`, `*.pem`, `*.key`, `id_*`, `*.pfx`, `credentials*`, `secrets*`), `adopt.exclude` and links are left out, and the prompt lists them. The view is read-only, so the inspected code cannot change.
- **Prompts** start with a short note naming `/work`, and `/memory` with its memory commit: the plan's pinned commit for a planned run, otherwise memory's commit when the first agent of the command starts. `{{prompt_file}}` becomes `/run/aeh/prompt.md`, and a profile with `stdin: true` reads that file.
- **Environment.** The container gets `HOME` (empty and private to the launch), the profile's `env` map and the host values of the names in its `sandbox.pass_env`. The project's `pass_env` is never forwarded, and `env` may refer only to names the profile forwards. Host paths such as `CODEX_HOME` or `CLAUDE_CONFIG_DIR` are not forwarded.
- **Network.** Agents use Docker's default network so their CLIs can reach their providers; the network is not filtered yet. Gates keep no network.
- **Sign-in.** Agents start with an empty home directory in this version, so a CLI must authenticate through variables in `sandbox.pass_env` (for example an API key). Keeping a CLI's login between launches is the next step.
- **Sealed tests you approved by hand** (`aeh tests approve`) must be committed exactly as approved: every sealed file tracked, no untracked file under the acceptance paths, and the same bytes in the commit (line endings converted by `core.autocrlf` are accepted). Containers run the committed bytes, so a test Git ignores or a filter rewrites would otherwise not be the one that runs.
- **Codex profiles** need `--skip-git-repo-check`, because `/work` is not a Git repository.
- **Failures.** If Docker or the image fails during a launch, the step blocks with the reason without consuming a ladder attempt. A triage that cannot finish for such a reason is not replaced by the default route: the command stops and triages again next time.

`aeh sandbox check` also runs a throwaway container from the agent image and warns about every profile whose program the image lacks. Each launch writes `<log>.launch.json` beside its output, with the class, exit code, image, network, the commit at `/work`, the memory commit and the captured candidate. The contracts of runs and plans pin policy 2, the agent image and every profile's `sandbox.pass_env`. Discovery sessions and adoption areas do not pin them: their outputs are documents you review, and each command uses the current configuration.

## A .NET example

```dockerfile
FROM mcr.microsoft.com/dotnet/sdk:8.0
ENV NUGET_PACKAGES=/opt/nuget DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
WORKDIR /opt/restore
# Copy only what restore needs (solution and project files), never secrets.
COPY *.sln ./
COPY src/ src/
RUN dotnet restore && chmod -R a+rX /opt/nuget && rm -rf /opt/restore
```

This example is a starting point; adapt it and check it with `aeh sandbox check`. With every package in `/opt/nuget`, `dotnet build` and `dotnet test` restore from it without the network. Configure commands such as `build: "dotnet build -warnaserror"` and `test: "dotnet test --no-build"`.

## Cleanup and recovery

Every container and volume carries `aeh.project` and `aeh.launch` labels. AEH records each launch before creating anything. When a run, plan or verification resumes, AEH removes what an interrupted launch left behind and confirms the removal. To see what exists: `docker ps -a --filter label=aeh.project` and `docker volume ls --filter label=aeh.project`.
