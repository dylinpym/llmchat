// Package httpin exposes the service over HTTP with Server-Sent Events.
package httpin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/dylin/llmchat/internal/contextpack"
	"github.com/dylin/llmchat/internal/domain"
	"github.com/dylin/llmchat/internal/observability"
	"github.com/dylin/llmchat/internal/provider"
	"github.com/dylin/llmchat/internal/service"
)

// Server wires routes to the service.
type Server struct {
	svc     *service.Service
	metrics *observability.Metrics
	health  HealthChecker
}

// HealthChecker reports dependency reachability for /readyz.
type HealthChecker interface {
	PingDB() error
	PingProvider() error
}

func NewServer(svc *service.Service, m *observability.Metrics, h HealthChecker) *Server {
	return &Server{svc: svc, metrics: m, health: h}
}

// Routes builds the public router.
func (s *Server) Routes(allowedOrigins []string) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.accessLog)
	r.Use(middleware.Recoverer)
	// Deliberately no middleware.Timeout here: it wraps ResponseWriter and would
	// break SSE, and a hard 60s cap would kill legitimate long generations.
	// Request lifetime is bounded by client disconnect plus the provider's own
	// stream deadline instead.

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Request-ID"},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/healthz", s.handleLiveness)
	r.Get("/readyz", s.handleReadiness)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.requireUser)

		r.Post("/conversations", s.handleCreateConversation)
		r.Get("/conversations", s.handleListConversations)
		r.Get("/conversations/{id}", s.handleGetConversation)
		r.Delete("/conversations/{id}", s.handleDeleteConversation)

		r.Get("/conversations/{id}/messages", s.handleListMessages)
		r.Get("/conversations/{id}/tree", s.handleTree)

		// The only streaming endpoint. POST is correct despite being a read-ish
		// operation: EventSource only supports GET, which cannot carry a body,
		// so clients use fetch + ReadableStream instead.
		r.Post("/conversations/{id}/messages", s.handleSendMessage)

		r.Patch("/messages/{id}", s.handleEditMessage)
		r.Post("/conversations/{id}/branch", s.handleSwitchBranch)
	})

	return r
}

// ------------------------------------------------------------------- handlers

type createConversationReq struct {
	Title        string `json:"title"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var req createConversationReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	conv, err := s.svc.Create(r.Context(), userID(r), req.Title, req.Model, req.SystemPrompt)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, conv)
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 20, 100)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0, 1<<30)

	owner := userID(r)
	list, err := s.svc.ListConversations(r.Context(), owner, limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "limit": limit, "offset": offset})
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	conv, err := s.svc.GetConversation(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteConversation(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	branch, err := s.svc.Branch(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": branchView(branch)})
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	tree, err := s.svc.Tree(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tree})
}

type sendMessageReq struct {
	Content  string `json:"content"`
	ParentID string `json:"parent_id"`
}

// handleSendMessage streams the assistant reply as Server-Sent Events.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var req sendMessageReq
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if err := s.serveSSE(w, r, flusher, ok, chi.URLParam(r, "id"), req); err != nil {
		// Only safe to write a status before the first flush.
		writeErr(w, err)
		return
	}
}

func (s *Server) serveSSE(
	w http.ResponseWriter,
	r *http.Request,
	flusher http.Flusher,
	ok bool,
	conversationID string,
	req sendMessageReq,
) error {
	if !ok {
		return fmt.Errorf("httpin: ResponseWriter does not support flushing")
	}

	stream, err := s.svc.Send(r.Context(), service.SendMessageInput{
		ConversationID: conversationID,
		Content:        req.Content,
		ParentID:       req.ParentID,
	})
	if err != nil {
		return err
	}

	// No buffering anywhere between us and the client: nginx and similar proxies
	// otherwise aggregate the stream and the user sees the whole reply at once.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)

	for chunk := range stream {
		kind := "delta"
		payload := map[string]any{"text": chunk.Content}
		switch {
		case chunk.Usage != nil:
			kind = "usage"
			payload = map[string]any{
				"prompt_tokens":     chunk.Usage.PromptTokens,
				"completion_tokens": chunk.Usage.CompletionTokens,
				"total_tokens":      chunk.Usage.TotalTokens,
			}
		case chunk.Done:
			kind = "done"
			payload = map[string]any{}
		}

		if err := writeEvent(w, enc, flusher, kind, payload); err != nil {
			return nil // client is gone; not an error worth logging as 5xx
		}
		if chunk.Done {
			return nil
		}
	}
	return nil
}

// writeEvent emits one SSE frame:
//
//	event: <kind>\n
//	data: <json>\n
//	\n
//
// The JSON payload is marshalled separately rather than streamed through the
// encoder, because json.Encoder.Encode appends a newline of its own and the
// frame layout would silently end up malformed.
func writeEvent(w io.Writer, _ *json.Encoder, f http.Flusher, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: "+event+"\ndata: "); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n\n"); err != nil {
		return err
	}
	f.Flush()
	return nil
}

func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	msg, err := s.svc.Edit(r.Context(), chi.URLParam(r, "id"), body.Content)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (s *Server) handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LeafID string `json:"leaf_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.svc.SwitchBranch(r.Context(), chi.URLParam(r, "id"), body.LeafID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	// Liveness must never depend on Postgres: a DB blip must not make the
	// orchestrator kill an otherwise healthy process and cascade the outage.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	status := http.StatusOK
	out := map[string]string{"status": "ok"}
	if err := s.health.PingDB(); err != nil {
		status = http.StatusServiceUnavailable
		out["db"] = "unavailable"
	}
	if err := s.health.PingProvider(); err != nil {
		out["provider"] = "degraded" // not fatal: cached conversations may still serve
	}
	writeJSON(w, status, out)
}

// ------------------------------------------------------------------ middleware

// requireUser is a placeholder principal resolver. Real auth means an API key or
// JWT here; it is deliberately not implemented so nobody mistakes the stub for
// finished security work.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		if uid == "" {
			writeErr(w, &apiError{Status: http.StatusUnauthorized, Msg: "missing X-User-Id"})
			return
		}
		next.ServeHTTP(w, withUserID(r, uid))
	})
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(rr, r)
		if s.metrics != nil {
			// chi.RouteContext is populated after the handler runs, so the
			// pattern (not the concrete path) is used as the label to keep
			// cardinality bounded.
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			s.metrics.ObserveRequest(route, r.Method, rr.Status(), time.Since(start))
		}
	})
}

// --------------------------------------------------------------------- helpers

// maxBodyBytes caps request bodies before reading. Without it, a client can post
// an unbounded JSON body and turn a chat endpoint into a memory-exhaustion
// vector; this is a real concern because the natural payload here is long text.
const maxBodyBytes = 1 << 20 // 1 MiB

func decode(r *http.Request, dst any) error {
	body := io.LimitReader(r.Body, maxBodyBytes+1)
	if err := json.NewDecoder(body).Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return &apiError{Status: http.StatusBadRequest, Msg: "empty body"}
		}
		return &apiError{Status: http.StatusBadRequest, Msg: "invalid JSON: " + err.Error()}
	}
	return nil
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, map[string]string{"error": ae.Msg})
		return
	}
	var up *provider.ErrUpstream
	if errors.As(err, &up) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "model backend unavailable"})
		return
	}
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, service.ErrLeafNotInConversation) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	var over *contextpack.OverflowError
	if errors.As(err, &over) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": over.Error()})
		return
	}
	// Anything else is our bug: log the detail, return a generic message so
	// internal error strings never reach clients.
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func atoiDefault(s string, def, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// branchView strips internal fields from the transcript the client renders.
func branchView(b domain.Branch) []map[string]any {
	out := make([]map[string]any, 0, len(b))
	for _, m := range b {
		view := map[string]any{
			"id":          m.ID,
			"role":        m.Role,
			"content":     m.Content,
			"token_count": m.TokenCount,
			"created_at":  m.CreatedAt,
		}
		if m.ParentID != nil {
			view["parent_id"] = *m.ParentID
		}
		if len(m.ToolCalls) > 0 {
			view["tool_calls"] = m.ToolCalls
		}
		out = append(out, view)
	}
	return out
}
