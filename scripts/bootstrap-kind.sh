#!/bin/bash
set -e

CLUSTER_NAME="k8s-buddy"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load environment variables from .env if it exists
if [ -f "${SCRIPT_DIR}/../.env" ]; then
    set -a
    source "${SCRIPT_DIR}/../.env"
    set +a
fi

# Validate required secrets
if [ -z "${GRAFANA_PASSWORD}" ]; then
    echo "❌ GRAFANA_PASSWORD is not set. Please create a .env file with GRAFANA_PASSWORD defined."
    echo "   See .env.example for the required format."
    exit 1
fi

echo "🚀 Starting k8s-buddy bootstrap..."
echo "========================================"

# Check prerequisites
echo "📋 Checking prerequisites..."
command -v kind >/dev/null 2>&1 || { echo "❌ kind is required but not installed."; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo "❌ kubectl is required but not installed."; exit 1; }
command -v helm >/dev/null 2>&1 || { echo "❌ helm is required but not installed."; exit 1; }

# Create kind cluster
echo "🔧 Creating kind cluster: ${CLUSTER_NAME}"
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "⚠️  Cluster '${CLUSTER_NAME}' already exists, using it..."
else
    kind create cluster --name "${CLUSTER_NAME}" --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30080
    hostPort: 30080
    protocol: TCP
  - containerPort: 30443
    hostPort: 30443
    protocol: TCP
EOF
fi

# Wait for cluster to be ready
echo "⏳ Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=120s

# Install observability stack
echo "📦 Installing observability stack..."

# Add Helm repos if not already added
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
helm repo add grafana https://grafana.github.io/helm-charts 2>/dev/null || true
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts 2>/dev/null || true
helm repo update

# Install kube-state-metrics
echo "📊 Installing kube-state-metrics..."
helm upgrade --install kube-state-metrics prometheus-community/kube-state-metrics \
    --namespace monitoring \
    --create-namespace \
    --set resources.requests.cpu="50m" \
    --set resources.requests.memory="64Mi" \
    --wait --timeout 60s

# Install Prometheus
echo "📈 Installing Prometheus..."
helm upgrade --install prometheus prometheus-community/prometheus \
    --namespace monitoring \
    --values "${SCRIPT_DIR}/../observability/helm-values/prometheus-values.yaml" \
    --wait --timeout 120s

# Install Loki
echo "📝 Installing Loki..."
helm upgrade --install loki grafana/loki \
    --namespace monitoring \
    --values "${SCRIPT_DIR}/../observability/helm-values/loki-values.yaml" \
    --wait --timeout 120s

# Install Tempo
echo "🔍 Installing Tempo..."
helm upgrade --install tempo open-telemetry/tempo \
    --namespace monitoring \
    --values "${SCRIPT_DIR}/../observability/helm-values/tempo-values.yaml" \
    --wait --timeout 120s

# Install OpenTelemetry Collector
echo "🌐 Installing OpenTelemetry Collector..."
helm upgrade --install otel-collector open-telemetry/opentelemetry-collector \
    --namespace monitoring \
    --values "${SCRIPT_DIR}/../observability/helm-values/otel-collector-values.yaml" \
    --wait --timeout 120s

# Install Grafana
echo "🎨 Installing Grafana..."
helm upgrade --install grafana grafana/grafana \
    --namespace monitoring \
    --set adminPassword="${GRAFANA_PASSWORD}" \
    --set persistence.enabled=false \
    --wait --timeout 120s

# Build and load Docker images
echo "🐳 Building and loading Docker images..."
IMAGES=("buddy-api" "buddy-worker" "chaos-buddy")
for IMAGE in "${IMAGES[@]}"; do
    echo "   Building ${IMAGE}..."
    docker build -t "${IMAGE}:latest" "${SCRIPT_DIR}/../apps/${IMAGE}/"
    echo "   Loading ${IMAGE} into kind..."
    kind load docker-image "${IMAGE}:latest" --name "${CLUSTER_NAME}"
done

# Deploy k8s-buddy applications
echo "🚀 Deploying k8s-buddy applications..."
kubectl apply -k "${SCRIPT_DIR}/../k8s/overlays/local-kind/"

# Wait for deployments
echo "⏳ Waiting for deployments to be ready..."
kubectl rollout status deployment/buddy-api -n k8s-buddy --timeout=120s || echo "⚠️  buddy-api rollout pending"
kubectl rollout status deployment/buddy-worker -n k8s-buddy --timeout=120s || echo "⚠️  buddy-worker rollout pending"
kubectl rollout status deployment/chaos-buddy -n k8s-buddy --timeout=120s || echo "⚠️  chaos-buddy rollout pending"

# Import Grafana dashboards
echo "📊 Importing Grafana dashboards..."
GRAFANA_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=grafana -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring "${GRAFANA_POD}" -- grafana-cli admin admin plugins install grafana-clock-panel 2>/dev/null || true
kubectl exec -n monitoring "${GRAFANA_POD}" -- grafana-cli admin admin plugins install grafana-piechart-panel 2>/dev/null || true

# Import the k8s-buddy dashboard
DASHBOARD_FILE="${SCRIPT_DIR}/../observability/dashboards/k8s-buddy-dashboard.json"
if [ -f "${DASHBOARD_FILE}" ]; then
    echo "📊 Importing k8s-buddy dashboard..."
    DASHBOARD=$(cat "${DASHBOARD_FILE}" | jq -c '.')
    kubectl exec -n monitoring "${GRAFANA_POD}" -- curl -s -X POST \
        -H "Content-Type: application/json" \
        -u "admin:admin" \
        "http://localhost:3000/api/dashboards/db" \
        -d "{\"dashboard\": ${DASHBOARD}, \"overwrite\": true, \"folderId\": 0}" || echo "⚠️  Dashboard import failed"
fi

# Add Loki datasource
echo "📝 Adding Loki datasource..."
kubectl exec -n monitoring "${GRAFANA_POD}" -- curl -s -X POST \
    -H "Content-Type: application/json" \
    -u "admin:admin" \
    "http://localhost:3000/api/datasources" \
    -d '{"name":"Loki","type":"loki","url":"http://loki:3100","access":"proxy","isDefault":true}' || echo "⚠️  Loki datasource creation failed"

# Add Tempo datasource
echo "🔍 Adding Tempo datasource..."
kubectl exec -n monitoring "${GRAFANA_POD}" -- curl -s -X POST \
    -H "Content-Type: application/json" \
    -u "admin:admin" \
    "http://localhost:3000/api/datasources" \
    -d '{"name":"Tempo","type":"tempo","url":"http://tempo:3100","access":"proxy"}' || echo "⚠️  Tempo datasource creation failed"

echo ""
echo "========================================"
echo "🎉 Bootstrap complete!"
echo "========================================"
echo ""
echo "📝 Useful commands:"
echo "   kubectl port-forward -n monitoring svc/grafana 3000:3000"
echo "   kubectl port-forward -n monitoring svc/prometheus-server 9090:80"
echo "   kubectl port-forward -n k8s-buddy svc/buddy-api 8080:8080"
echo ""
echo "🔗 URLs (after port-forward):"
echo "   Grafana: http://localhost:3000 (admin, see .env for password)"
echo "   Prometheus: http://localhost:9090"
echo "   buddy-api: http://localhost:8080"
echo ""
echo "🚀 Run './scripts/demo-chaos.sh' to see chaos in action!"
