package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

//go:embed *.gohtml
var templates embed.FS

// Config holds all application configuration loaded from environment variables.
type Config struct {
	// Container resolution — see docker.go for priority order.
	MCContainerName string // explicit override; skips all label-based lookup when set
	MCStackName     string // Compose stack name; auto-derived from own labels when empty
	MCServiceName   string // Compose service name of the MC container (default "minecraft")

	AuthPasswordHash string
	JWTSecret        string
	Port             string
	WorldPath        string // optional override; empty means auto-detect
	MCUID            int    // uid to chown the world to after upload (default 1000)
	MCGID            int    // gid to chown the world to after upload (default 1000)
}

func loadConfig() Config {
	_ = godotenv.Load()

	return Config{
		MCContainerName:  getEnv("MC_CONTAINER_NAME", ""),
		MCStackName:      getEnv("MC_STACK_NAME", ""),
		MCServiceName:    getEnv("MC_SERVICE_NAME", "minecraft"),
		AuthPasswordHash: getEnv("AUTH_PASSWORD_HASH", ""),
		JWTSecret:        getEnv("JWT_SECRET", ""),
		Port:             getEnv("PORT", "8080"),
		WorldPath:        getEnv("WORLD_PATH", ""),
		MCUID:            getEnvInt("MC_UID", 1000),
		MCGID:            getEnvInt("MC_GID", 1000),
	}
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// writeJSON writes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorJSON writes a JSON error response.
func errorJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func main() {
	// genpw subcommand: hash a password and print it, then exit.
	if len(os.Args) == 2 && os.Args[1] == "genpw" {
		fmt.Print("Password: ")
		pw, err := readPassword()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading password: %v\n", err)
			os.Exit(1)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error hashing password: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	cfg := loadConfig()

	if cfg.AuthPasswordHash == "" {
		log.Fatal("AUTH_PASSWORD_HASH is required")
	}
	if cfg.JWTSecret == "" {
		log.Fatal("JWT_SECRET is required")
	}

	dockerSvc, err := newDockerService(cfg)
	if err != nil {
		log.Fatalf("failed to connect to Docker: %v", err)
	}
	defer dockerSvc.close()

	tmpl, err := template.ParseFS(templates, "*.gohtml")
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}

	mux := http.NewServeMux()

	// Public routes
	mux.Handle("GET /{$}", pageHandler(tmpl, "status.gohtml", cfg))
	mux.Handle("GET /login", pageHandler(tmpl, "index.gohtml", cfg))
	mux.Handle("GET /api/public/status", publicStatusHandler(dockerSvc))
	mux.HandleFunc("GET /health", healthHandler())
	mux.HandleFunc("POST /login", loginHandler(cfg))
	mux.HandleFunc("POST /logout", logoutHandler())

	// Protected routes
	mux.Handle("GET /dashboard", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasSession(cfg, r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		pageHandler(tmpl, "index.gohtml", cfg).ServeHTTP(w, r)
	}))
	mux.Handle("GET /api/status", jwtMiddleware(cfg, statusHandler(dockerSvc)))
	mux.Handle("POST /api/start", jwtMiddleware(cfg, startHandler(dockerSvc)))
	mux.Handle("POST /api/stop", jwtMiddleware(cfg, stopHandler(dockerSvc)))
	mux.Handle("POST /api/restart", jwtMiddleware(cfg, restartHandler(dockerSvc)))
	mux.Handle("GET /api/world/download", jwtMiddleware(cfg, downloadHandler(dockerSvc, cfg)))
	mux.Handle("POST /api/world/upload", jwtMiddleware(cfg, uploadHandler(dockerSvc, cfg)))

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // disabled for streaming download
		IdleTimeout:  60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("minedash listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}

	log.Println("stopped")
}

// healthHandler returns 200 OK to confirm the service is running.
func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// pageHandler renders the page with the validated session state.
func pageHandler(tmpl *template.Template, name string, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := tmpl.ExecuteTemplate(w, name, struct{ Authenticated bool }{hasSession(cfg, r)}); err != nil {
			log.Printf("template error: %v", err)
		}
	})
}

// publicStatusHandler exposes only the state, keeping Docker errors private.
func publicStatusHandler(d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		state, err := d.status(r.Context())
		if err != nil {
			log.Printf("public status: %v", err)
			errorJSON(w, http.StatusServiceUnavailable, "status unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": state})
	})
}

// logRequest logs the action and the real remote address.
func logRequest(r *http.Request, action string) {
	realIP := r.Header.Get("X-Forwarded-For")
	if realIP == "" {
		realIP = r.Header.Get("X-Real-IP")
	}
	if realIP == "" {
		realIP = r.RemoteAddr
	}
	log.Printf("%s: request from %s", action, realIP)
}

// statusHandler returns the current MC container state.
func statusHandler(d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, err := d.status(r.Context())
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to get status: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": state})
	})
}

// startHandler starts the MC container.
func startHandler(d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logRequest(r, "start")
		if err := d.start(r.Context()); err != nil {
			log.Printf("start: failed: %v", err)
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to start: %v", err))
			return
		}
		log.Printf("start: container started successfully")
		writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
	})
}

// stopHandler stops the MC container gracefully.
func stopHandler(d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logRequest(r, "stop")
		if err := d.stop(r.Context()); err != nil {
			log.Printf("stop: failed: %v", err)
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to stop: %v", err))
			return
		}
		log.Printf("stop: container stopped successfully")
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
	})
}

// restartHandler restarts the MC container.
func restartHandler(d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logRequest(r, "restart")
		if err := d.restart(r.Context()); err != nil {
			log.Printf("restart: failed: %v", err)
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to restart: %v", err))
			return
		}
		log.Printf("restart: container restarted successfully")
		writeJSON(w, http.StatusOK, map[string]string{"status": "restarted"})
	})
}

// downloadHandler streams the world folder as a ZIP file.
// The server must be stopped before downloading.
func downloadHandler(d *dockerService, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logRequest(r, "download")
		state, err := d.status(r.Context())
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to get status: %v", err))
			return
		}
		if state != "stopped" {
			log.Printf("download: rejected — server is %s", state)
			errorJSON(w, http.StatusConflict, "server must be stopped before downloading the world")
			return
		}

		worldPath, err := resolveWorldPath(cfg)
		if err != nil {
			log.Printf("download: world not found: %v", err)
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("world not found: %v", err))
			return
		}

		filename := fmt.Sprintf("world-%s.zip", time.Now().UTC().Format("2006-01-02T15-04-05"))
		log.Printf("download: streaming %s from %s", filename, worldPath)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		w.Header().Set("Content-Type", "application/zip")

		if err := streamWorldZip(worldPath, w); err != nil {
			log.Printf("download: stream error: %v", err)
			return
		}
		log.Printf("download: complete: %s", filename)
	})
}

// uploadHandler receives a ZIP and atomically replaces the world folder.
// The server must be stopped before uploading.
func uploadHandler(d *dockerService, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logRequest(r, "upload")
		state, err := d.status(r.Context())
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to get status: %v", err))
			return
		}
		if state != "stopped" {
			log.Printf("upload: rejected — server is %s", state)
			errorJSON(w, http.StatusConflict, "server must be stopped before uploading a world")
			return
		}

		worldPath, err := resolveWorldPath(cfg)
		if err != nil {
			log.Printf("upload: world not found: %v", err)
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("world not found: %v", err))
			return
		}

		if err := receiveWorldUpload(r, worldPath, cfg.MCUID, cfg.MCGID); err != nil {
			log.Printf("upload: failed: %v", err)
			errorJSON(w, http.StatusBadRequest, fmt.Sprintf("upload failed: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "uploaded"})
	})
}

// readPassword reads a password from stdin.
// When stdin is a terminal the input is hidden (no echo); otherwise it reads a plain line
// so the command can also be used non-interactively (e.g. echo "pw" | ./minedash genpw).
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println() // newline after the hidden input
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// stdin is a pipe / redirect — read a plain line
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	return strings.TrimRight(scanner.Text(), "\r\n"), scanner.Err()
}
