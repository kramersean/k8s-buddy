# k8s-buddy

Kubernetes chaos engineering companion for observability testing.

## Prerequisites

- kind
- kubectl
- helm
- docker

## Setup

1. **Install prerequisites** (if needed):
   ```bash
   # Example for Ubuntu/Debian
   curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.20.0/kind-linux-amd64
   chmod +x ./kind && sudo mv ./kind /usr/local/bin/kind

   curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
   chmod +x ./kubectl && sudo mv ./kubectl /usr/local/bin/kubectl

   curl https://baltocdn.com/helm/signing.asc | gpg --dearmor | sudo tee /usr/share/keyrings/helm.gpg > /dev/null
   echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/helm.gpg] https://baltocdn.com/helm/debian/ all main" | sudo tee /etc/apt/sources.list.d/helm.list > /dev/null
   sudo apt-get update && sudo apt-get install helm
   ```

2. **Configure environment**:
   ```bash
   cp .env.example .env
   # Edit .env and set GRAFANA_PASSWORD to a secure value
   ```

3. **Run bootstrap**:
   ```bash
   ./scripts/bootstrap-kind.sh
   ```

## Accessing Services

After running bootstrap, use port-forwarding:

```bash
# Grafana
kubectl port-forward -n monitoring svc/grafana 3000:3000

# Prometheus
kubectl port-forward -n monitoring svc/prometheus-server 9090:80

# buddy-api
kubectl port-forward -n k8s-buddy svc/buddy-api 8080:8080
```

**Default URLs:**
- Grafana: http://localhost:3000
- Prometheus: http://localhost:9090
- buddy-api: http://localhost:8080

## Updating Deployment

To pull latest changes and redeploy:

```bash
git pull && ./scripts/bootstrap-kind.sh
```
