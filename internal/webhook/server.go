package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxBodyBytes = 1 << 20

type ReconcileFunc func(ctx context.Context)

type Config struct {
	Addr   string
	Path   string
	Secret string
}

type Server struct {
	cfg       Config
	reconcile ReconcileFunc
	log       *slog.Logger
	triggers  chan struct{}
}

func New(cfg Config, reconcile ReconcileFunc, log *slog.Logger) *Server {
	return &Server{
		cfg:       cfg,
		reconcile: reconcile,
		log:       log,
		triggers:  make(chan struct{}, 1),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+s.cfg.Path, s.handleWebhook)
	return mux
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	workerCtx, stopWorker := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		stopWorker()
		wg.Wait()
	}()
	wg.Go(func() { s.processTriggers(workerCtx) })

	errCh := make(chan error, 1)
	wg.Go(func() { errCh <- srv.Serve(ln) })

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) processTriggers(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.triggers:
			s.reconcile(ctx)
		}
	}
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusBadRequest)
		return
	}

	if !s.validSignature(r.Header.Get("X-Hub-Signature-256"), body) {
		s.log.Warn("webhook: rejected request with invalid signature", "remote_addr", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	select {
	case s.triggers <- struct{}{}:
	default:
		s.log.Info("webhook: reconcile already queued, coalescing trigger")
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) validSignature(header string, body []byte) bool {
	if s.cfg.Secret == "" {
		return true
	}

	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}

	mac := hmac.New(sha256.New, []byte(s.cfg.Secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(sig), []byte(expected))
}
