package guard

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Prushka/arr-guard/internal/arr"
)

func (s *Service) WebhookHandler(kind string) http.HandlerFunc {
	client := s.arr[kind]
	return func(w http.ResponseWriter, r *http.Request) {
		if client == nil {
			http.NotFound(w, r)
			return
		}
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload arr.WebhookPayload
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		if err := decoder.Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if !strings.EqualFold(payload.EventType, "download") && !strings.EqualFold(payload.EventType, "importcomplete") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, _, err := storedWebhook(kind, payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Enqueue(client, payload); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *Service) authorized(r *http.Request) bool {
	tokenConfigured := s.config.WebhookToken != ""
	basicConfigured := s.config.WebhookUsername != "" || s.config.WebhookPassword != ""
	if !tokenConfigured && !basicConfigured {
		return true
	}
	if tokenConfigured {
		value := strings.TrimSpace(r.Header.Get("X-Webhook-Token"))
		if value == "" {
			authorization := strings.TrimSpace(r.Header.Get("Authorization"))
			if len(authorization) >= len("Bearer ") && strings.EqualFold(authorization[:len("Bearer ")], "Bearer ") {
				value = strings.TrimSpace(authorization[len("Bearer "):])
			}
		}
		if subtle.ConstantTimeCompare([]byte(value), []byte(s.config.WebhookToken)) == 1 {
			return true
		}
	}
	if basicConfigured {
		username, password, ok := r.BasicAuth()
		if ok && subtle.ConstantTimeCompare([]byte(username), []byte(s.config.WebhookUsername)) == 1 && subtle.ConstantTimeCompare([]byte(password), []byte(s.config.WebhookPassword)) == 1 {
			return true
		}
	}
	return false
}

func (s *Service) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.config.ListenAddr)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	return s.serveOnListener(ctx, listener)
}

func (s *Service) serveOnListener(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Bind successfully before workers are allowed to run any recovery work.
	s.StartWorkers(ctx)
	defer s.StopWorkers()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	for kind := range s.arr {
		mux.HandleFunc("/webhook/"+kind, s.WebhookHandler(kind))
	}
	server := &http.Server{Addr: s.config.ListenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	s.log.Info("listening", "addr", s.config.ListenAddr)
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
