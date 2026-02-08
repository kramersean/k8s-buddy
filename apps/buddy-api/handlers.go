package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("buddy-api-handlers")

type HealthResponse struct {
	Status    string `json:"status"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// HealthzHandler returns a playful health check response
// @Summary Health check endpoint
// @Description Returns a playful health message indicating the service is running
// @Tags health
// @Produce json
// @Success 200 {object} HealthResponse
// @Router /healthz [get]
func HealthzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HealthzHandler")
	defer span.End()

	span.SetAttributes(attribute.String("handler", "healthz"))

	response := HealthResponse{
		Status:    "healthy",
		Message:   "🎉 I feel great! All systems go for maximum awesome!",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// ReadyzHandler returns a playful readiness check response
// @Summary Readiness check endpoint
// @Description Returns a playful readiness message indicating if the service is ready to receive traffic
// @Tags health
// @Produce json
// @Success 200 {object} HealthResponse
// @Success 503 {object} HealthResponse
// @Router /readyz [get]
func ReadyzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "ReadyzHandler")
	defer span.End()

	span.SetAttributes(attribute.String("handler", "readyz"))

	response := HealthResponse{
		Status:    "ready",
		Message:   "🚀 Ready to rock and roll! Let's do this thing!",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// HomeHandler returns the home page with status and links
// @Summary Home page
// @Description Returns the home page with service status and available endpoints
// @Tags default
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router / [get]
func HomeHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HomeHandler")
	defer span.End()

	span.SetAttributes(attribute.String("handler", "home"))

	response := map[string]interface{}{
		"status":  "alive",
		"message": "🎉 Welcome to Buddy API! Your friendly neighborhood microservice.",
		"endpoints": map[string]string{
			"/":       "Home - this page",
			"/healthz": "Health check - are we feeling good?",
			"/readyz": "Readiness check - ready to work?",
			"/work":   "Do some work (with optional chaos)",
			"/metrics": "Prometheus metrics",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// WorkHandler processes work requests with optional failure modes
// @Summary Work endpoint
// @Description Triggers controlled errors based on environment variables
// @Tags work
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Success 500 {object} map[string]interface{}
// @Router /work [post]
func WorkHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "WorkHandler")
	defer span.End()

	span.SetAttributes(attribute.String("handler", "work"))

	response := map[string]interface{}{
		"status":   "success",
		"message":  "✨ Work completed successfully! High five! ✨",
		"work_id":  time.Now().UnixNano(),
		"duration": time.Since(time.Now()).String(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func RegisterRoutes(r *chi.Mux) {
	r.Get("/", HomeHandler)
	r.Get("/healthz", HealthzHandler)
	r.Get("/readyz", ReadyzHandler)
	r.Post("/work", WorkHandler)
}
