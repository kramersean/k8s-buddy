#!/bin/bash

echo "🚀 k8s-buddy Port Forward Helper"
echo "================================="
echo ""
echo "Select a service to port-forward:"
echo ""
echo "1) Grafana          (http://localhost:3000) - Dashboards & logs"
echo "2) Prometheus       (http://localhost:9090) - Metrics"
echo "3) Loki             (http://localhost:3100) - Logs"
echo "4) Tempo            (http://localhost:16686) - Traces"
echo "5) buddy-api        (http://localhost:8080) - Main API"
echo "6) buddy-worker     (http://localhost:8081) - Worker"
echo "7) chaos-buddy      (http://localhost:8082) - Chaos control"
echo "8) All services"
echo ""
echo "q) Quit"
echo ""

read -p "👉 Enter your choice: " choice

case $choice in
    1)
        echo "🔌 Starting Grafana..."
        kubectl port-forward -n monitoring svc/grafana 3000:3000
        ;;
    2)
        echo "🔌 Starting Prometheus..."
        kubectl port-forward -n monitoring svc/prometheus-server 9090:80
        ;;
    3)
        echo "🔌 Starting Loki..."
        kubectl port-forward -n monitoring svc/loki 3100:3100
        ;;
    4)
        echo "🔌 Starting Tempo..."
        kubectl port-forward -n monitoring svc/tempo 16686:16686
        ;;
    5)
        echo "🔌 Starting buddy-api..."
        kubectl port-forward -n k8s-buddy svc/buddy-api 8080:8080
        ;;
    6)
        echo "🔌 Starting buddy-worker..."
        kubectl port-forward -n k8s-buddy svc/buddy-worker 8081:8081
        ;;
    7)
        echo "🔌 Starting chaos-buddy..."
        kubectl port-forward -n k8s-buddy svc/chaos-buddy 8082:8082
        ;;
    8)
        echo "🔌 Starting all services (Ctrl+C to stop)..."
        kubectl port-forward -n monitoring svc/grafana 3000:3000 &
        kubectl port-forward -n monitoring svc/prometheus-server 9090:80 &
        kubectl port-forward -n monitoring svc/loki 3100:3100 &
        kubectl port-forward -n monitoring svc/tempo 16686:16686 &
        kubectl port-forward -n k8s-buddy svc/buddy-api 8080:8080 &
        kubectl port-forward -n k8s-buddy svc/buddy-worker 8081:8081 &
        kubectl port-forward -n k8s-buddy svc/chaos-buddy 8082:8082 &
        wait
        ;;
    q|Q)
        echo "👋 Goodbye!"
        exit 0
        ;;
    *)
        echo "❌ Invalid choice!"
        exit 1
        ;;
esac
