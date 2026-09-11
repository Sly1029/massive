#!/usr/bin/env bash
set -euo pipefail
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repository"
kind="${KIND:-kind}"
export KUBECTL="${KUBECTL:-kubectl}"
scratch="$(mktemp -d)"
cluster="massive-conformance-$$"
export KUBECONFIG="$scratch/kubeconfig"
logs="$repository/dist/argo-test-logs"
mkdir -p "$logs" "$scratch/image"
cleanup() {
  result=$?
  if (( result != 0 )); then
    "$KUBECTL" -n argo get workflows,pods -o yaml > "$logs/resources.yaml" 2>&1 || true
    "$KUBECTL" -n argo logs deployment/workflow-controller > "$logs/controller.log" 2>&1 || true
    "$KUBECTL" -n argo logs -l workflows.argoproj.io/workflow --all-containers --prefix --max-log-requests=50 --tail=200 > "$logs/pods.log" 2>&1 || true
  fi
  "$kind" delete cluster --name "$cluster"
  rm -rf "$scratch"
  exit "$result"
}
trap cleanup EXIT
uv sync --frozen --project packages/python
export MASSIVE_PYTHON="$repository/packages/python/.venv/bin/python"
uv build --wheel --out-dir "$scratch/image" packages/python
uv export --frozen --project packages/python --no-dev --no-emit-project \
  --format requirements-txt --output-file "$scratch/image/requirements.txt" > /dev/null
cp packages/python/Dockerfile "$scratch/image/Dockerfile"
image="massive-conformance:run-$$"
docker build -t "$image" "$scratch/image"
"$kind" create cluster --name "$cluster" --image kindest/node:v1.34.0 --wait 60s
"$kind" load docker-image "$image" --name "$cluster"
# Tag the imported manifest by digest as well, so the immutable reference can
# run without a registry or any remote image publication.
digest="$(docker exec "$cluster-control-plane" ctr -n k8s.io images list | awk -v image="docker.io/library/$image" '$1 == image { print $3 }')"
export MASSIVE_TEST_ARGO_IMAGE="docker.io/library/massive-conformance@$digest"
docker exec "$cluster-control-plane" ctr -n k8s.io images tag "docker.io/library/$image" "$MASSIVE_TEST_ARGO_IMAGE"
export MASSIVE_TEST_ARGO_PLATFORM="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"
curl -fsSL https://raw.githubusercontent.com/argoproj/argo-workflows/v3.7.16/manifests/quick-start-minimal.yaml \
  -o "$scratch/install.yaml"
# The tagged source manifest deliberately uses :latest for development images.
# Pin controller/server explicitly; the controller derives the executor version.
sed -i 's|argoproj/argocli:latest|argoproj/argocli:v3.7.16|g; s|argoproj/workflow-controller:latest|argoproj/workflow-controller:v3.7.16|g' "$scratch/install.yaml"
"$KUBECTL" create namespace argo
"$KUBECTL" apply -n argo -f "$scratch/install.yaml"
"$KUBECTL" rollout status -n argo deployment/workflow-controller --timeout=120s
"$KUBECTL" rollout status -n argo deployment/minio --timeout=120s
"$MASSIVE_PYTHON" conformance/argo/test_cluster.py
