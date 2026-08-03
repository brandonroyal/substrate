# Sandbox Demo

This directory contains a demo of a stateful sandbox execution environment running on Agent Substrate.

It allows you to run arbitrary commands in an sandboxed, isolated container (running Alpine Linux) and preserves the execution state across suspends and resumes.

> [!WARNING]
> **Security Disclaimer:** This demo is not secured by any authorization checks, and the sandbox actor will execute any client-provided commands with no validation. Do not deploy this configuration in production or expose it to untrusted networks.

## Components

1.  **Sandbox Server (`main.go`)**: The application that runs inside the Agent Substrate actor. It exposes a simple, stateless `/process` endpoint to execute commands.
2.  **Sandbox Client (`client/`)**: A CLI REPL tool that allows you to interact with the sandbox actor interactively.

## Prerequisites

- A k8s cluster with Agent Substrate installed.
- `ko` installed for building images.
- A GCS bucket for storing snapshots (configured in `demos/sandbox/sandbox.yaml.tmpl`).
- `kubectl-ate` CLI installed (can be installed via `go install ./cmd/kubectl-ate`).

## How to Run on Agent Substrate

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `demos/sandbox/sandbox.yaml.tmpl`. The installation script automatically injects your `${BUCKET_NAME}` environment variable during deployment.

Use the core installation script to build the image and apply the resolved manifests to your cluster:

```bash
./hack/install-ate.sh --deploy-demo-sandbox
```

This command will:
- Build the sandbox server image based on Alpine Linux.
- Create the `ate-demo-sandbox` namespace.
- Create the `WorkerPool` and `ActorTemplate`.

Wait until the template is ready:
```bash
kubectl wait --for=condition=Ready actortemplate/sandbox-template -n ate-demo-sandbox --timeout=5m
```

### 2. Create a Sandbox Actor

Actors live in an **atespace**, which must exist before you create actors in it. Create one (e.g., `demo`), then create the sandbox actor with a chosen ID (e.g., `my-sandbox-1`):

```bash
# Install the CLI as a kubectl plugin if not already installed
go install ./cmd/kubectl-ate

# Create the atespace (required before creating actors).
kubectl ate create atespace demo

# Create the actor in the atespace, using the sandbox template.
kubectl ate create actor my-sandbox-1 -a demo --template ate-demo-sandbox/sandbox-template
```

### 3. Port-Forward Services

If running clients locally, port-forward the API and router in separate terminals:

```bash
# Terminal 1: API Server
kubectl port-forward -n ate-system svc/api 8080:443

# Terminal 2: Router
kubectl port-forward -n ate-system svc/atenet-router 8000:80
```

## How to Use the Client

Build and run the client REPL:

```bash
go build -o bin/sandbox-client ./demos/sandbox/client

./bin/sandbox-client --ateapi=localhost:8080 --atenet=localhost:8000 --atespace=demo --id=my-sandbox-1
```

Once in the `sandbox>` prompt, you can run commands:

```bash
sandbox> ls -la
sandbox> pwd
sandbox> echo "Hello" > test.txt
sandbox> cat test.txt
```

Type `exit` to leave. This will automatically trigger the suspension of the actor.

## How to Run the Multiplexing Demo

Before running the demo, ensure the sandbox demo is deployed:
```bash
./hack/install-ate.sh --deploy-demo-sandbox
```

Then run the automated multiplexing script:
```bash
./demos/sandbox/run_multiplex_demo.sh
```

Alternatively, you can run the demo inside a split-pane `tmux` session to watch the state transitions and pod stability in real time:
```bash
./demos/sandbox/run_tmux_demo.sh
```
This splits your terminal:
- **Left Panel**: Runs the automated multiplexing client.
- **Top Right Panel**: Watches Kubernetes pods in the sandbox namespace.
- **Bottom Right Panel**: Watches active worker assignments on Substrate.


This automated demo script will:
1. Port-forward the API and router services.
2. Query the initial state of the worker pods (showing 200 replicas).
3. Create 1000 sandbox actors (`sandbox-1` through `sandbox-1000`).
4. Sequentially write a unique state file containing a specific string (e.g., `state-value-for-actor-i`) to each of the 1000 actors, suspending each actor immediately after writing.
5. Sequentially read back the state file from each actor to verify that its unique state was preserved across the suspend/resume cycles.
6. Print the state of the worker pods after the demonstration, proving that they did not restart or churn during the process.

If you need to manually clean up and delete all actors in the `sandbox-multiplex` atespace (e.g., if a run failed halfway or you want to reset), run:

```bash
./demos/sandbox/cleanup_multiplex_demo.sh
```

## How to Uninstall

To remove the sandbox demo resources (namespace, workerpool, and template) from your cluster, run:

```bash
./hack/install-ate.sh --delete-demo-sandbox
```
