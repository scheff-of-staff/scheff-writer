// Command scheff-writer serves the writing surface over a folder of
// Markdown projects. Configuration is environment-only; see README.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scheff-of-staff/scheff-writer/internal/llm"
	"github.com/scheff-of-staff/scheff-writer/internal/store"
	"github.com/scheff-of-staff/scheff-writer/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := env("WRITER_LISTEN", "127.0.0.1:8445")
	root := env("WRITER_ROOT", "")
	if root == "" {
		log.Fatal("WRITER_ROOT is required: the folder that holds 04_Projects/ (and optionally 06_Reference/)")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		log.Fatalf("WRITER_ROOT %q is not a readable directory", root)
	}
	st := store.New(root)
	if v := os.Getenv("WRITER_PROJECTS_DIR"); v != "" {
		st.ProjectsDir = v
	}
	if v := os.Getenv("WRITER_REFERENCE_DIR"); v != "" {
		st.ReferenceDir = v
	}

	model := llm.New(os.Getenv("WRITER_LLM_URL"), os.Getenv("WRITER_LLM_KEY"), os.Getenv("WRITER_MODEL"))
	if model.BaseURL == "" || model.APIKey == "" || model.Model == "" {
		log.Print("warn: WRITER_LLM_URL / WRITER_LLM_KEY / WRITER_MODEL not all set; Transform and Chat will report an error")
	}

	token := os.Getenv("WRITER_TOKEN")
	noAuth := os.Getenv("WRITER_ALLOW_NO_AUTH") == "1"
	if token != "" && len(token) < 16 {
		log.Fatal("WRITER_TOKEN must be at least 16 characters")
	}
	if token == "" && noAuth {
		log.Print("warn: WRITER_ALLOW_NO_AUTH=1, so anyone who can reach this port can edit. Bind to localhost only.")
	}

	apiToken := os.Getenv("WRITER_API_TOKEN")
	var apiModels []string
	if v := os.Getenv("WRITER_API_MODELS"); v != "" {
		apiModels = strings.Split(v, ",")
	}
	apiMax := 0
	if v := os.Getenv("WRITER_API_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			log.Fatal("WRITER_API_MAX_CONCURRENT must be a positive integer")
		}
		apiMax = n
	}
	if apiToken != "" {
		log.Print("JSON API on at /api/v1/ (WRITER_API_TOKEN set)")
	}

	h, err := web.New(web.Config{
		Store:          st,
		LLM:            model,
		Token:          token,
		AllowNoAuth:    noAuth,
		InsecureCookie: os.Getenv("WRITER_INSECURE_COOKIE") == "1",
		HomeURL:        os.Getenv("WRITER_HOME_URL"),
		ProjectURL:     os.Getenv("WRITER_PROJECT_URL"),

		APIToken:         apiToken,
		APIModels:        apiModels,
		APIMaxConcurrent: apiMax,
	})
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      200 * time.Second, // Transform can take minutes on a long draft
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("scheff-writer listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
