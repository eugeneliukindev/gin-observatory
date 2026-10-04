set shell := ["bash", "-uc"]

# Compose lives beside the Kubernetes manifests, under deploy/.
export COMPOSE_FILE := "deploy/compose/docker-compose.yaml"
# go.mod asks for Go 1.26: an older go command fetches that toolchain instead of refusing.
export GOTOOLCHAIN := "auto"

_cluster := "go-observatory"
_kubectl := "kubectl --context k3d-" + _cluster + " --namespace " + _cluster

[doc("All command information")]
default:
    @just --list --unsorted --list-heading $'Available commands…\n'

[group("setup")]
[doc("Download the modules")]
install:
    go mod download

[group("setup")]
[doc("Install git hooks via prek")]
hooks:
    prek install

[group("lint")]
[doc("Lint and format: all, or one of golangci · fmt · tidy")]
[arg("tool", pattern="all|golangci|fmt|tidy", help="what to run; all by default")]
lint tool="all":
    @just _lint-{{ tool }}

[group("lint")]
[doc("Run the tests")]
test:
    go test ./...

[group("stack")]
[doc("The stack in Docker Compose: up · down · ps · logs, then what docker compose takes")]
[arg("command", pattern="up|down|ps|logs", help="up, down, ps or logs")]
dc command *args:
    @just _dc-{{ command }} {{ args }}

alias docker-compose := dc

# The same host ports as Compose — 3000, 8000, 12345, 12347: stop one before starting the other.
[group("stack")]
[doc("The stack in a local k3d cluster: up · down · ps · logs")]
[arg("command", pattern="up|down|ps|logs", help="up, down, ps or logs")]
k3d command *args:
    @just _k3d-{{ command }} {{ args }}

[group("stack")]
[doc("Drive traffic at the app: RATE=10 DURATION=60 just traffic")]
traffic:
    ./traffic.sh

# Outside Docker, against a stack already running.
[group("stack")]
[doc("Run the app here")]
run:
    go run ./cmd/api

[private]
_lint-all: _lint-fmt _lint-tidy _lint-golangci

[private]
_lint-fmt:
    golangci-lint fmt

[private]
_lint-golangci:
    golangci-lint run

# go.mod and go.sum list what the code imports, no more and no less.
[private]
_lint-tidy:
    go mod tidy -diff

[private]
_dc-up *services:
    docker compose up --build -d {{ services }}

[private]
_dc-down *args:
    docker compose down {{ args }}

[private]
_dc-ps *args:
    docker compose ps {{ args }}

[private]
_dc-logs *services:
    docker compose logs -f {{ services }}

[private]
_k3d-up:
    k3d cluster list {{ _cluster }} >/dev/null 2>&1 || k3d cluster create {{ _cluster }} --wait \
        --port 3000:3000@loadbalancer --port 8000:8000@loadbalancer \
        --port 12345:12345@loadbalancer --port 12347:12347@loadbalancer
    docker build --tag go-observatory-api:dev .
    k3d image import go-observatory-api:dev --cluster {{ _cluster }}
    kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/kubernetes \
        | kubectl --context k3d-{{ _cluster }} apply --server-side --force-conflicts -f -
    # The tag stays `dev`: without a restart the pods keep the image they started with.
    {{ _kubectl }} rollout restart deployment/api
    {{ _kubectl }} rollout status deployment/api --timeout=5m
    {{ _kubectl }} rollout status statefulset --timeout=5m

# The data lives in the cluster: deleting it wipes everything.
[private]
_k3d-down:
    k3d cluster delete {{ _cluster }}

[private]
_k3d-ps:
    {{ _kubectl }} get pods

# Every workload carries app.kubernetes.io/name, the name of its Compose service.
[private]
_k3d-logs *services:
    {{ _kubectl }} logs -f --prefix --all-containers --max-log-requests=20 \
        --selector '{{ if services == "" { "app.kubernetes.io/name" } else { "app.kubernetes.io/name in (" + replace(services, " ", ",") + ")" } }}'
