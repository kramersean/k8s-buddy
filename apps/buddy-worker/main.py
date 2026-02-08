from flask import Flask, jsonify
import requests
import time
import logging
import random
import threading
from prometheus_client import Counter, Histogram, generate_latest, CONTENT_TYPE_LATEST
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.resources import Resource
from opentelemetry.semconv.resource import ResourceAttributes
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.trace import Status, StatusCode

app = Flask(__name__)

# Logging configuration
logging.basicConfig(
    level=logging.INFO,
    format='{"timestamp": "%(asctime)s", "level": "%(levelname)s", "message": "%(message)s"}'
)
logger = logging.getLogger(__name__)

# Prometheus metrics
WORKER_CHECKS_TOTAL = Counter(
    'buddy_worker_checks_total',
    'Total number of worker checks',
    ['check_type', 'status']
)

WORKER_CHECK_DURATION = Histogram(
    'buddy_worker_check_duration_seconds',
    'Duration of worker checks in seconds',
    ['check_type'],
    buckets=[0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0]
)

WORKER_ERRORS_TOTAL = Counter(
    'buddy_worker_errors_total',
    'Total number of worker errors',
    ['error_type']
)

# OpenTelemetry setup
resource = Resource.create({
    ResourceAttributes.SERVICE_NAME: "buddy-worker",
    ResourceAttributes.SERVICE_VERSION: "1.0.0"
})

tracer_provider = TracerProvider(resource=resource)
otlp_exporter = OTLPSpanExporter(endpoint="localhost:4317", insecure=True)
tracer_provider.add_span_processor(BatchSpanProcessor(otlp_exporter))
trace.set_tracer_provider(tracer_provider)
tracer = trace.get_tracer("buddy-worker")

# Playful messages
PLAYFUL_MESSAGES = {
    "check_start": "🔍 Taking a look around... what do we have here?",
    "check_complete": "✅ Check complete! Everything looks good!",
    "check_failed": "❌ Oops! Something went sideways!",
    "heartbeat": "💓 Worker is alive and kicking!",
}


def background_check():
    """Perform periodic background checks."""
    while True:
        try:
            with tracer.start_as_current_span("background_check") as span:
                span.set_attribute("check.type", "background")
                
                logger.info(PLAYFUL_MESSAGES["check_start"])
                
                # Simulate various checks
                check_types = ["health", "connectivity", "resources"]
                for check_type in check_types:
                    with tracer.start_as_current_span(f"{check_type}_check") as check_span:
                        check_span.set_attribute("check.name", check_type)
                        
                        start_time = time.time()
                        try:
                            # Simulate check work
                            time.sleep(random.uniform(0.1, 0.5))
                            duration = time.time() - start_time
                            
                            WORKER_CHECK_DURATION.labels(check_type=check_type).observe(duration)
                            WORKER_CHECKS_TOTAL.labels(check_type=check_type, status="success").inc()
                            
                            check_span.set_status(Status(StatusCode.OK))
                            check_span.set_attribute("check.result", "success")
                            
                            logger.info(f"✨ {check_type} check passed!")
                            
                        except Exception as e:
                            WORKER_CHECKS_TOTAL.labels(check_type=check_type, status="failed").inc()
                            WORKER_ERRORS_TOTAL.labels(error_type=str(e)).inc()
                            check_span.set_status(Status(StatusCode.ERROR, str(e)))
                            check_span.set_attribute("check.result", "failed")
                            logger.error(f"💥 {check_type} check failed: {e}")
                
                span.set_attribute("background_check.complete", True)
                
        except Exception as e:
            logger.error(f"💥 Background check error: {e}")
        
        # Sleep for 30 seconds before next check
        time.sleep(30)


@app.route('/healthz')
def healthz():
    """Health check endpoint."""
    with tracer.start_as_current_span("healthz") as span:
        span.set_attribute("handler", "healthz")
        
        response = {
            "status": "healthy",
            "message": "🎉 I feel great! All systems go for maximum awesome!",
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        }
        
        span.set_status(Status(StatusCode.OK))
        logger.info(PLAYFUL_MESSAGES["heartbeat"])
        
        return jsonify(response), 200


@app.route('/readyz')
def readyz():
    """Readiness check endpoint."""
    with tracer.start_as_current_span("readyz") as span:
        span.set_attribute("handler", "readyz")
        
        response = {
            "status": "ready",
            "message": "🚀 Ready to rock and roll! Let's do this thing!",
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        }
        
        span.set_status(Status(StatusCode.OK))
        logger.info("✅ Worker is ready!")
        
        return jsonify(response), 200


@app.route('/')
def home():
    """Home page endpoint."""
    with tracer.start_as_current_span("home") as span:
        span.set_attribute("handler", "home")
        
        response = {
            "status": "alive",
            "message": "🎉 Welcome to Buddy Worker! Your friendly background processor.",
            "endpoints": {
                "/": "Home - this page",
                "/healthz": "Health check - are we feeling good?",
                "/readyz": "Readiness check - ready to work?",
                "/metrics": "Prometheus metrics",
                "/trigger-check": "Trigger a manual check"
            }
        }
        
        span.set_status(Status(StatusCode.OK))
        
        return jsonify(response), 200


@app.route('/trigger-check')
def trigger_check():
    """Trigger a manual check."""
    with tracer.start_as_current_span("trigger_check") as span:
        span.set_attribute("handler", "trigger_check")
        
        try:
            # Perform a check
            check_type = "manual"
            start_time = time.time()
            time.sleep(random.uniform(0.1, 0.3))
            duration = time.time() - start_time
            
            WORKER_CHECK_DURATION.labels(check_type=check_type).observe(duration)
            WORKER_CHECKS_TOTAL.labels(check_type=check_type, status="success").inc()
            
            span.set_status(Status(StatusCode.OK))
            span.set_attribute("check.duration", duration)
            
            response = {
                "status": "success",
                "message": "✨ Manual check completed! You rock!",
                "duration_seconds": duration,
                "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            }
            
            return jsonify(response), 200
            
        except Exception as e:
            WORKER_CHECKS_TOTAL.labels(check_type="manual", status="failed").inc()
            WORKER_ERRORS_TOTAL.labels(error_type=str(e)).inc()
            
            span.set_status(Status(StatusCode.ERROR, str(e)))
            
            response = {
                "status": "error",
                "message": "😈 Oops! Manual check failed!",
                "error": str(e),
                "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            }
            
            return jsonify(response), 500


@app.route('/metrics')
def metrics():
    """Prometheus metrics endpoint."""
    return generate_latest(), 200, {'Content-Type': CONTENT_TYPE_LATEST}


def start_background_thread():
    """Start the background check thread."""
    thread = threading.Thread(target=background_check, daemon=True)
    thread.start()
    logger.info("🚀 Background check thread started!")
    return thread


if __name__ == '__main__':
    start_background_thread()
    
    port = int(os.environ.get('PORT', 8081))
    logger.info(f"🚀 buddy-worker starting on port {port}")
    logger.info("📊 Metrics available at /metrics")
    
    app.run(host='0.0.0.0', port=port, debug=False)
