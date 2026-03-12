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
	MCContainerName  string
	AuthPasswordHash string
	JWTSecret        string
	Port             string
	WorldPath        string // optional override; empty means auto-detect
}

func loadConfig() Config {
	_ = godotenv.Load()

	return Config{
		MCContainerName:  getEnv("MC_CONTAINER_NAME", "minecraft"),
		AuthPasswordHash: getEnv("AUTH_PASSWORD_HASH", ""),
		JWTSecret:        getEnv("JWT_SECRET", ""),
		Port:             getEnv("PORT", "8080"),
		WorldPath:        getEnv("WORLD_PATH", ""),
	}
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

	dockerSvc, err := newDockerService(cfg.MCContainerName)
	if err != nil {
		log.Fatalf("failed to connect to Docker: %v", err)
	}
	defer dockerSvc.close()

	tmpl, err := template.ParseFS(templates, "index.gohtml")
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}

	mux := http.NewServeMux()

	// Public routes
	mux.HandleFunc("POST /login", loginHandler(cfg))
	mux.HandleFunc("POST /logout", logoutHandler())

	// Protected routes
	mux.Handle("GET /{$}", indexHandler(tmpl))
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

// indexHandler serves the main UI page.
func indexHandler(tmpl *template.Template) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, nil); err != nil {
			log.Printf("template error: %v", err)
		}
	})
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
		if err := d.start(r.Context()); err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to start: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
	})
}

// stopHandler stops the MC container gracefully.
func stopHandler(d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := d.stop(r.Context()); err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to stop: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
	})
}

// restartHandler restarts the MC container.
func restartHandler(d *dockerService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := d.restart(r.Context()); err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to restart: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "restarted"})
	})
}

// downloadHandler streams the world folder as a ZIP file.
// The server must be stopped before downloading.
func downloadHandler(d *dockerService, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, err := d.status(r.Context())
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to get status: %v", err))
			return
		}
		if state != "stopped" {
			errorJSON(w, http.StatusConflict, "server must be stopped before downloading the world")
			return
		}

		worldPath, err := resolveWorldPath(cfg)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("world not found: %v", err))
			return
		}

		filename := fmt.Sprintf("world-%s.zip", time.Now().UTC().Format("2006-01-02T15-04-05"))
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		w.Header().Set("Content-Type", "application/zip")

		if err := streamWorldZip(worldPath, w); err != nil {
			log.Printf("world zip stream error: %v", err)
		}
	})
}

// uploadHandler receives a ZIP and atomically replaces the world folder.
// The server must be stopped before uploading.
func uploadHandler(d *dockerService, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, err := d.status(r.Context())
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("failed to get status: %v", err))
			return
		}
		if state != "stopped" {
			errorJSON(w, http.StatusConflict, "server must be stopped before uploading a world")
			return
		}

		worldPath, err := resolveWorldPath(cfg)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, fmt.Sprintf("world not found: %v", err))
			return
		}

		if err := receiveWorldUpload(r, worldPath); err != nil {
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
