package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	namespace           = "k8s-buddy"
	configMapName       = "buddy-config"
	chaosEnabledEnv     = "CHAOS_ENABLED"
	defaultChaosEnabled = false
	killIntervalEnv     = "KILL_INTERVAL_SECONDS"
	defaultKillInterval = 60
)

var (
	clientset  *kubernetes.Clientset
	cfg        *chaosConfig
	mu         sync.Mutex
	chaosMode  bool
	stopChan   chan struct{}
	configData string
)

type chaosConfig struct {
	chaosEnabled  bool
	killInterval  int
	podsToKill     []string
}

func init() {
	cfg = &chaosConfig{
		chaosEnabled:  getEnvBool(chaosEnabledEnv, defaultChaosEnabled),
		killInterval:  getEnvInt(killIntervalEnv, defaultKillInterval),
	}
	stopChan = make(chan struct{})
	rand.Seed(time.Now().UnixNano())
}

func getEnvBool(key string, defaultVal bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val == "true" || val == "1"
}

func getEnvInt(key string, defaultVal int) int {
	var val int
	_, err := fmt.Sscanf(os.Getenv(key), "%d", &val)
	if err != nil {
		return defaultVal
	}
	return val
}

func setupClient() error {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("Using local config: %v", err)
		config = &rest.Config{
			Host: "https://localhost:6443",
		}
	}

	clientset, err = kubernetes.NewForConfig(config)
	return err
}

func getPodsToKill() ([]string, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var podNames []string
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			podNames = append(podNames, pod.Name)
		}
	}

	return podNames, nil
}

func killRandomPod() error {
	mu.Lock()
	enabled := cfg.chaosEnabled
	mu.Unlock()

	if !enabled {
		return nil
	}

	pods, err := getPodsToKill()
	if err != nil {
		return fmt.Errorf("failed to get pods: %w", err)
	}

	if len(pods) == 0 {
		log.Println("😴 No pods to kill right now...")
		return nil
	}

	target := pods[rand.Intn(len(pods))]
	log.Printf("🎯 Targeting pod: %s", target)

	err = clientset.CoreV1().Pods(namespace).Delete(context.Background(), target, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete pod %s: %w", target, err)
	}

	log.Printf("💥 Pod %s has been terminated! Goodbye, sweet prince! 👻", target)
	return nil
}

func flipConfigMap() error {
	mu.Lock()
	enabled := cfg.chaosEnabled
	mu.Unlock()

	if !enabled {
		return nil
	}

	// Toggle config data to trigger readiness failures
	if configData == "primary" {
		configData = "chaos"
	} else {
		configData = "primary"
	}

	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	cm.Data["mode"] = configData
	_, err = clientset.CoreV1().ConfigMaps(namespace).Update(context.Background(), cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ConfigMap: %w", err)
	}

	log.Printf("🔄 ConfigMap %s flipped to '%s' mode! Shaking things up!", configMapName, configData)
	return nil
}

func chaosLoop() {
	ticker := time.NewTicker(time.Duration(cfg.killInterval) * time.Second)
	defer ticker.Stop()

	log.Printf("🚀 Chaos loop started! Interval: %d seconds", cfg.killInterval)

	for {
		select {
		case <-stopChan:
			log.Println("🛑 Chaos loop stopped!")
			return
		case <-ticker.C:
			mu.Lock()
			enabled := cfg.chaosEnabled
			mu.Unlock()

			if !enabled {
				continue
			}

			// Randomly choose an action
			if rand.Float64() < 0.7 {
				if err := killRandomPod(); err != nil {
					log.Printf("❌ Failed to kill pod: %v", err)
				}
			} else {
				if err := flipConfigMap(); err != nil {
					log.Printf("❌ Failed to flip ConfigMap: %v", err)
				}
			}
		}
	}
}

func toggleHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	wasEnabled := chaosMode
	chaosMode = !chaosMode
	cfg.chaosEnabled = chaosMode
	mu.Unlock()

	status := "disabled"
	if chaosMode {
		status = "enabled"
	}

	log.Printf("🔀 Chaos mode toggled: %s → %s", status, !status)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status": "success", "chaos_enabled": %v, "message": "Chaos mode %s! Let's make some trouble!"}`, chaosMode, status)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	enabled := cfg.chaosEnabled
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"chaos_enabled": %v, "namespace": "%s", "kill_interval_seconds": %d}`, enabled, namespace, cfg.killInterval)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	enabled := cfg.chaosEnabled
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{
		"status": "alive",
		"message": "👋 Welcome to Chaos Buddy! Your friendly neighborhood troublemaker!",
		"chaos_enabled": %v,
		"endpoints": {
			"/": "This page",
			"/status": "Current chaos status",
			"/toggle": "Toggle chaos mode on/off"
		}
	}`, enabled)
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status": "healthy", "message": "🎉 I feel great! All systems go for maximum awesome!"}`)
}

func main() {
	if err := setupClient(); err != nil {
		log.Fatalf("Failed to setup client: %v", err)
	}

	mu.Lock()
	chaosMode = cfg.chaosEnabled
	mu.Unlock()

	go chaosLoop()

	// HTTP server for control
	mux := http.NewServeMux()
	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/toggle", toggleHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		log.Printf("🌪️ Chaos Buddy started on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Server error: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	close(stopChan)
	srv.Close()
	log.Println("👋 Chaos Buddy shutting down!")
}
