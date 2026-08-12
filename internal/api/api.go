// Package api exposes the check engine over HTTP.
//
// The shape follows Zanzibar's: Check answers one question, Write mutates
// relationships and returns a consistency token, Expand explains a decision.
// Every response that depends on a snapshot carries the token for that
// snapshot, so a client can make its next read no older than what it has
// already seen.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/APD007/zanzo/internal/check"
	"github.com/APD007/zanzo/internal/storage"
)

type Server struct {
	Engine *check.Engine
	Store  storage.Store
	// Observe is called once per request with the outcome. Injected rather
	// than imported so the package carries no metrics dependency.
	Observe func(route string, status int, d time.Duration)
	// Logger records the causes of 5xx responses. Clients get a generic
	// message -- a caller that can tell "no such relation" from "denied"
	// learns the schema -- but swallowing the cause entirely leaves the
	// service undebuggable, which is how a 500 becomes permanent.
	Logger *log.Logger
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

// StatusClientClosed is nginx's 499. A caller that hangs up mid-check is not
// a server fault and must not be counted as one; under load testing those
// cancellations are the difference between a real error budget and noise.
const StatusClientClosed = 499

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/check", s.instrument("/v1/check", s.handleCheck))
	mux.HandleFunc("POST /v1/write", s.instrument("/v1/write", s.handleWrite))
	mux.HandleFunc("POST /v1/list-objects", s.instrument("/v1/list-objects", s.handleListObjects))
	mux.HandleFunc("POST /v1/expand", s.instrument("/v1/expand", s.handleExpand))
	mux.HandleFunc("GET /healthz", s.instrument("/healthz", s.handleHealth))
	return mux
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) instrument(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		if s.Observe != nil {
			s.Observe(route, sw.status, time.Since(start))
		}
	}
}

type checkRequest struct {
	Object   string `json:"object"`
	Relation string `json:"relation"`
	Subject  string `json:"subject"`
	// Consistency is "minimize_latency" (default), "at_least_as_fresh" or
	// "full". at_least_as_fresh requires a token.
	Consistency string `json:"consistency,omitempty"`
	Token       string `json:"token,omitempty"`
}

type checkResponse struct {
	Allowed bool   `json:"allowed"`
	Token   string `json:"token"`
	// Debug numbers, useful when explaining why a check was slow.
	Expansions int `json:"expansions"`
	CacheHits  int `json:"cache_hits"`
	MemoHits   int `json:"memo_hits"`
	MaxDepth   int `json:"max_depth"`
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return
	}
	object, err := storage.ParseObject(req.Object)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	subject, err := storage.ParseSubject(req.Subject)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Relation == "" {
		writeError(w, http.StatusBadRequest, "relation is required")
		return
	}

	cr := check.Request{Object: object, Relation: req.Relation, Subject: subject}
	switch req.Consistency {
	case "", "minimize_latency":
		cr.Consistency = check.MinimizeLatency
	case "full":
		cr.Consistency = check.FullConsistency
	case "at_least_as_fresh":
		if req.Token == "" {
			writeError(w, http.StatusBadRequest, "at_least_as_fresh requires a token")
			return
		}
		rev, err := parseToken(req.Token)
		if err != nil {
			writeError(w, http.StatusBadRequest, "token is not valid")
			return
		}
		cr.Consistency = check.AtLeastAsFresh
		cr.Token = check.Token{Revision: rev}
	default:
		writeError(w, http.StatusBadRequest, "consistency must be minimize_latency, at_least_as_fresh or full")
		return
	}

	res, err := s.Engine.Check(r.Context(), cr)
	if err != nil {
		// A malformed schema reference is the caller's fault; anything else is
		// ours. Neither leaks the reason, because a probe that can distinguish
		// "no such relation" from "denied" learns about the schema.
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The caller hung up or timed out. Nothing was decided and nobody
			// is listening, so record it as a client-closed request rather
			// than inflating the server error rate.
			s.logf("check %s#%s@%s: client went away: %v",
				req.Object, req.Relation, req.Subject, err)
			writeError(w, StatusClientClosed, "client closed request")
		case errors.Is(err, check.ErrDepthExceeded):
			writeError(w, http.StatusUnprocessableEntity, "check exceeded maximum depth")
		default:
			s.logf("check %s#%s@%s: %v", req.Object, req.Relation, req.Subject, err)
			writeError(w, http.StatusInternalServerError, "check failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, checkResponse{
		Allowed:    res.Allowed,
		Token:      formatToken(res.Revision),
		Expansions: res.Expansions,
		CacheHits:  res.CacheHits,
		MemoHits:   res.MemoHits,
		MaxDepth:   res.MaxDepth,
	})
}

type relationship struct {
	Object   string `json:"object"`
	Relation string `json:"relation"`
	Subject  string `json:"subject"`
}

type writeRequest struct {
	Add    []relationship `json:"add,omitempty"`
	Remove []relationship `json:"remove,omitempty"`
}

type writeResponse struct {
	Token string `json:"token"`
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return
	}
	if len(req.Add) == 0 && len(req.Remove) == 0 {
		writeError(w, http.StatusBadRequest, "write requires at least one of add or remove")
		return
	}
	add, err := toTuples(req.Add)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	remove, err := toTuples(req.Remove)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rev, err := s.Store.Write(r.Context(), add, remove)
	if err != nil {
		s.logf("write (add=%d remove=%d): %v", len(add), len(remove), err)
		writeError(w, http.StatusInternalServerError, "write failed")
		return
	}
	// The caller gets the token for the revision their write created. Handing
	// it back on the next check is what stops a just-revoked grant from being
	// served out of some older snapshot.
	writeJSON(w, http.StatusOK, writeResponse{Token: formatToken(rev)})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Head(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "store unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func toTuples(rs []relationship) ([]storage.Tuple, error) {
	var out []storage.Tuple
	for _, r := range rs {
		object, err := storage.ParseObject(r.Object)
		if err != nil {
			return nil, err
		}
		subject, err := storage.ParseSubject(r.Subject)
		if err != nil {
			return nil, err
		}
		if r.Relation == "" {
			return nil, errors.New("relation is required on every relationship")
		}
		out = append(out, storage.Tuple{Object: object, Relation: r.Relation, Subject: subject})
	}
	return out, nil
}

// Tokens are opaque to clients by contract. They are a bare revision today;
// keeping the encoding behind these two functions means adding a shard id or a
// signature later does not change the API.
func formatToken(rev storage.Revision) string {
	return "zk-" + strconv.FormatUint(uint64(rev), 10)
}

func parseToken(s string) (storage.Revision, error) {
	if len(s) < 4 || s[:3] != "zk-" {
		return 0, errors.New("bad token")
	}
	n, err := strconv.ParseUint(s[3:], 10, 64)
	if err != nil {
		return 0, err
	}
	return storage.Revision(n), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type listRequest struct {
	Subject    string `json:"subject"`
	Permission string `json:"permission"`
	ObjectType string `json:"object_type"`
	Limit      int    `json:"limit,omitempty"`
}

type listResponse struct {
	Objects []string `json:"objects"`
	Token   string   `json:"token"`
	// Candidates exposes how many objects the reverse walk proposed before
	// verification. A caller does not need it; an operator watching the
	// reverse index earn or fail to earn its write cost does.
	Candidates int `json:"candidates"`
}

func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request) {
	var req listRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return
	}
	subject, err := storage.ParseSubject(req.Subject)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Permission == "" || req.ObjectType == "" {
		writeError(w, http.StatusBadRequest, "permission and object_type are required")
		return
	}
	// An unbounded list is a denial-of-service against ourselves, so the API
	// caps what the library leaves open.
	if req.Limit <= 0 || req.Limit > 1000 {
		req.Limit = 1000
	}

	res, err := s.Engine.ListObjects(r.Context(), check.ListRequest{
		Subject:    subject,
		Permission: req.Permission,
		ObjectType: req.ObjectType,
		Limit:      req.Limit,
	})
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeError(w, StatusClientClosed, "client closed request")
		default:
			s.logf("list-objects %s %s/%s: %v", req.Subject, req.ObjectType, req.Permission, err)
			writeError(w, http.StatusInternalServerError, "list failed")
		}
		return
	}
	objects := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		objects = append(objects, o.String())
	}
	writeJSON(w, http.StatusOK, listResponse{
		Objects:    objects,
		Token:      formatToken(res.Revision),
		Candidates: res.Candidates,
	})
}

type expandRequest struct {
	Object   string `json:"object"`
	Relation string `json:"relation"`
}

type expandResponse struct {
	Tree  *check.TreeNode `json:"tree"`
	Token string          `json:"token"`
}

func (s *Server) handleExpand(w http.ResponseWriter, r *http.Request) {
	var req expandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return
	}
	object, err := storage.ParseObject(req.Object)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Relation == "" {
		writeError(w, http.StatusBadRequest, "relation is required")
		return
	}
	res, err := s.Engine.Expand(r.Context(), check.ExpandRequest{Object: object, Relation: req.Relation})
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeError(w, StatusClientClosed, "client closed request")
		case errors.Is(err, check.ErrDepthExceeded):
			writeError(w, http.StatusUnprocessableEntity, "expand exceeded maximum depth")
		default:
			s.logf("expand %s#%s: %v", req.Object, req.Relation, err)
			writeError(w, http.StatusInternalServerError, "expand failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, expandResponse{Tree: res.Tree, Token: formatToken(res.Revision)})
}
