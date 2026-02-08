package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var (
	healthChecks = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "buddy_api_health_checks_total",
			Help: "Total number of health checks",
		},
		[]string{"status"},
	)

	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "buddy_api_requests_total",
			Help: "Total number of requests",
		},
		[]string{"method", "endpoint", "status"},
	)

	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "buddy_api_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "endpoint"},
	)

	errorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "buddy_api_errors_total",
			Help: "Total number of errors by type",
		},
		[]string{"type"},
	)

	tracer = otel.Tracer("buddy-api")
)

type failureConfig struct {
	random500Rate           float64
	latencyMs               int
	readinessFailDuration   int
	crashLoopAfterN         int
	cpuBurnSeconds          int
}

var (
	cfg                failureConfig
	requestCount       int
	mu                 sync.Mutex
	ready              bool = true
	readinessFailStart time.Time
	cpuBurnStart       time.Time
	cpuBurning         bool
)

const (
	playfulHealthMessage  = "🎉 I feel great! All systems go for maximum awesome!"
	playfulReadyMessage   = "🚀 Ready to rock and roll! Let's do this thing!"
	playfulNotReadyMessage = "😴 Taking a quick nap... be right back!"
)

func init() {
	prometheus.MustRegister(healthChecks, requestsTotal, requestDuration, errorsTotal)
	rand.Seed(time.Now().UnixNano())
}

func loadConfig() {
	cfg.random500Rate, _ = os.LookupEnv("RANDOM_500_RATE")
	if val := os.Getenv("RANDOM_500_RATE"); val != "" {
		fmt.Sscanf(val, "%f", &cfg.random500Rate)
	}
	fmt.Sscanf(os.Getenv("LATENCY_MS"), "%d", &cfg.latencyMs)
	fmt.Sscanf(os.Getenv("READINESS_FAIL_DURATION_SECONDS"), "%d", &cfg.readinessFailDuration)
	fmt.Sscanf(os.Getenv("CRASH_LOOP_AFTER_N_REQUESTS"), "%d", &cfg.crashLoopAfterN)
	fmt.Sscanf(os.Getenv("CPU_BURN_SECONDS"), "%d", &cfg.cpuBurnSeconds)
}

func setupOTEL() {
	ctx := context.Background()
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		log.Printf("OTEL setup error: %v", err)
		return
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("buddy-api"),
			semconv.ServiceVersion("1.0.0"),
		)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "home")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "alive",
		"message": "🎉 Welcome to Buddy API! Your friendly neighborhood microservice.",
		"endpoints": map[string]string{
			"/":       "Home - this page",
			"/healthz": "Health check - are we feeling good?",
			"/readyz": "Readiness check - ready to work?",
			"/work":   "Do some work (with optional chaos)",
			"/metrics": "Prometheus metrics",
		},
	})
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "healthz")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	healthChecks.WithLabelValues("healthy").Inc()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "healthy",
		"message": playfulHealthMessage,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func readyzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "readyz")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")

	mu.Lock()
	isReady := ready
	mu.Unlock()

	if !isReady {
		w.WriteHeader(http.StatusServiceUnavailable)
		healthChecks.WithLabelValues("not_ready").Inc()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "not_ready",
			"message": playfulNotReadyMessage,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	healthChecks.WithLabelValues("ready").Inc()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ready",
		"message": playfulReadyMessage,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func workHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "work")
	defer span.End()

	start := time.Now()
	defer func() {
		requestDuration.WithLabelValues("POST", "/work").Observe(time.Since(start).Seconds())
	}()

	mu.Lock()
	requestCount++
	crashCount := cfg.crashLoopAfterN
	mu.Unlock()

	span.SetAttributes(attribute.Int("request_number", requestCount))

	// Check for crash loop
	if crashCount > 0 && requestCount%crashCount == 0 {
		errorsTotal.WithLabelValues("crash_loop").Inc()
		log.Println("💥 Simulating crash loop!")
		os.Exit(1)
	}

	// Check for CPU burn
	mu.Lock()
	cpuBurn := cfg.cpuBurnSeconds
	mu.Unlock()
	if cpuBurn > 0 {
		go burnCPU(cpuBurn)
	}

	// Add latency if configured
	if cfg.latencyMs > 0 {
		jitter := rand.Intn(cfg.latencyMs / 2)
		time.Sleep(time.Duration(cfg.latencyMs+jitter) * time.Millisecond)
	}

	// Random 500 errors
	if cfg.random500Rate > 0 && rand.Float64() < cfg.random500Rate {
		errorsTotal.WithLabelValues("random_500").Inc()
		span.SetAttributes(attribute.String("error.type", "random_500"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "error",
			"message": "😈 Oops! Random error gotcha!",
			"type":    "random_500",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "success",
		"message":  "✨ Work completed successfully! High five! ✨",
		"work_id":  requestCount,
		"duration": time.Since(start).String(),
	})
}

func burnCPU(seconds int) {
	mu.Lock()
	cpuBurning = true
	cpuBurnStart = time.Now()
	mu.Unlock()

	end := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(end) {
		_ = 0
		for i := 0; i < 10000000; i++ {
			_ = i * i
		}
	}

	mu.Lock()
	cpuBurning = false
	mu.Unlock()
}

func readinessManager() {
	for {
		mu.Lock()
		if cfg.readinessFailDuration > 0 && !ready {
			if time.Since(readinessFailStart) > time.Duration(cfg.readinessFailDuration)*time.Second {
				ready = true
				log.Println("🌟 Readiness restored! Back in action!")
			}
		}
		mu.Unlock()
		time.Sleep(time.Second)
	}
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}

func main() {
	loadConfig()
	setupOTEL()

	r := chi.NewRouter()

	r.Use(otelhttp.Middleware("buddy-api"))
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			duration := time.Since(start)
			status := http.StatusOK
			if rw, ok := w.(interface{ WriteHeader(int) }); ok {
				// Can't easily get status, defaulting to 200 for metrics
			}
			requestsTotal.WithLabelValues(r.Method, r.URL.Path, "200").Inc()
		})
	})

	r.Get("/", homeHandler)
	r.Get("/healthz", healthzHandler)
	r.Get("/readyz", readyzHandler)
	r.Post("/work", workHandler)
	r.Get("/metrics", metricsHandler)

	go readinessManager()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🚀 buddy-api starting on port %s", port)
	log.Printf("📊 Metrics available at /metrics")
	log.Printf("🏥 Health at /healthz")
	log.Printf("✅ Ready at /readyz")

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("🛑 Shutting down gracefully...")
	srv.Close()
	log.Println("👋 Bye bye!")
}
