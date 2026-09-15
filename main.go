package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed web/index.html web/style.css
var webFiles embed.FS

type app struct {
	store    siteStore
	checker  *checker
	limiter  *rateLimiter
	routes   http.Handler
	template *template.Template
}

type suggestion struct {
	Name string
	URL  string
}

type pageData struct {
	Sites       []Site
	Suggestions []suggestion
	Prefill     string
	Error       string
	Added       bool
}

var suggestions = []suggestion{
	{Name: "GitHub", URL: "https://github.com/"},
	{Name: "Cloudflare", URL: "https://www.cloudflare.com/"},
	{Name: "Azure", URL: "https://azure.microsoft.com/"},
	{Name: "AWS", URL: "https://aws.amazon.com/"},
}

func newApp(store siteStore, checker *checker) *app {
	staticFiles, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}

	page := template.Must(template.New("index.html").Funcs(template.FuncMap{
		"formatTime": formatTime,
		"isoTime":    isoTime,
	}).ParseFS(webFiles, "web/index.html"))

	app := &app{
		store:    store,
		checker:  checker,
		limiter:  newRateLimiter(),
		template: page,
	}
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))
	mux.HandleFunc("/healthz", app.handleHealth)
	mux.HandleFunc("/api/sites", app.handleAPI)
	mux.HandleFunc("/sites", app.handleFormAdd)
	mux.HandleFunc("/", app.handleHome)
	app.routes = mux
	return app
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodPost && (r.URL.Path == "/sites" || r.URL.Path == "/api/sites") && !a.limiter.Allow(clientIP(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		if r.URL.Path == "/api/sites" {
			apiError(w, http.StatusTooManyRequests, "too many site checks; try again in a minute")
		} else {
			http.Error(w, "too many site checks; try again in a minute", http.StatusTooManyRequests)
		}
		return
	}
	a.routes.ServeHTTP(w, r)
}

func (a *app) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	a.renderHome(w, r, http.StatusOK, "")
}

func (a *app) handleFormAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		a.renderHome(w, r, http.StatusBadRequest, "Enter a valid web address.")
		return
	}

	if _, err := a.add(r.Context(), r.PostForm.Get("url")); err != nil {
		a.renderHome(w, r, statusForError(err), friendlyError(err))
		return
	}
	http.Redirect(w, r, "/?added=1", http.StatusSeeOther)
}

func (a *app) handleAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sites, err := a.store.List(r.Context())
		if err != nil {
			apiError(w, http.StatusInternalServerError, "could not load sites")
			return
		}
		writeJSON(w, http.StatusOK, sites)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		defer r.Body.Close()

		var input struct {
			URL string `json:"url"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			apiError(w, http.StatusBadRequest, "expected a JSON object with a url")
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			apiError(w, http.StatusBadRequest, "request body must contain one JSON object")
			return
		}

		site, err := a.add(r.Context(), input.URL)
		if err != nil {
			apiError(w, statusForError(err), friendlyError(err))
			return
		}
		writeJSON(w, http.StatusCreated, site)
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Health(ctx); err != nil {
		apiError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) add(ctx context.Context, rawURL string) (Site, error) {
	target, err := normalizeURL(rawURL)
	if err != nil {
		return Site{}, err
	}

	site, err := a.store.Create(ctx, target)
	if err != nil {
		return Site{}, err
	}
	return a.checker.Check(ctx, site)
}

func (a *app) renderHome(w http.ResponseWriter, r *http.Request, status int, errorMessage string) {
	sites, err := a.store.List(r.Context())
	if err != nil {
		http.Error(w, "could not load sites", http.StatusInternalServerError)
		return
	}

	prefill := strings.TrimSpace(r.URL.Query().Get("url"))
	if len(prefill) > maxURLLength {
		prefill = ""
	}
	a.render(w, status, pageData{
		Sites:       sites,
		Suggestions: suggestions,
		Prefill:     prefill,
		Error:       errorMessage,
		Added:       r.URL.Query().Get("added") == "1",
	})
}

func (a *app) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.template.ExecuteTemplate(w, "index.html", data); err != nil {
		log.Printf("render page: %v", err)
	}
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON: %v", err)
	}
}

func apiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func statusForError(err error) int {
	switch {
	case errors.Is(err, errInvalidURL):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func friendlyError(err error) string {
	switch {
	case errors.Is(err, errInvalidURL):
		return err.Error()
	default:
		log.Printf("add site: %v", err)
		return "Could not save that site. Please try again."
	}
}

func formatTime(value *time.Time) string {
	if value == nil {
		return "Not checked yet"
	}
	return value.UTC().Format("02 Jan 2006, 15:04 UTC")
}

func isoTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return duration, nil
}

func main() {
	interval, err := envDuration("CHECK_INTERVAL", 5*time.Minute)
	if err != nil {
		log.Fatal(err)
	}
	timeout, err := envDuration("CHECK_TIMEOUT", 10*time.Second)
	if err != nil {
		log.Fatal(err)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupCtx, cancel := context.WithTimeout(rootCtx, 15*time.Second)
	store, mode, err := openStore(startupCtx, os.Getenv("DATABASE_URL"))
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	checker := newChecker(store, timeout)
	runScheduler(rootCtx, checker, interval)
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           newApp(store, checker),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("upornot listening on http://localhost%s with %s storage", server.Addr, mode)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
		}
	case <-rootCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
