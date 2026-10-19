package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// scope 和 session 只允许字母数字、下划线、短横线，杜绝 ../ 之类的路径穿越
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// validName 校验 scope / session，session 允许为空
func validName(w http.ResponseWriter, field, value string, optional bool) bool {
	if (optional && value == "") || namePattern.MatchString(value) {
		return true
	}
	writeError(w, http.StatusBadRequest, "invalid "+field)
	return false
}

func newHandler(cfg *Config, m *Manager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("POST /v1/exec", requireKey(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleExec(cfg, m, w, r)
	})))
	// 销毁整个 scope 的容器，?purge=true 时同时删除 scope 的全部数据
	mux.Handle("DELETE /v1/scopes/{scope}", requireKey(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleRemove(m, w, r)
	})))
	// 销毁某次对话的容器，?purge=true 时同时删除这个对话的目录
	mux.Handle("DELETE /v1/scopes/{scope}/sessions/{session}", requireKey(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleRemove(m, w, r)
	})))
	registerFileRoutes(mux, cfg, m)
	return rejectNetworks(m.egressNets, mux)
}

// requireKey 校验 X-API-Key 或 Authorization: Bearer <key>
func requireKey(cfg *Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.APIKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleExec(cfg *Config, m *Manager, w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	var req ExecRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !validName(w, "scope", req.Scope, false) || !validName(w, "session", req.Session, true) {
		return
	}
	if _, ok := interpreters[req.Language]; !ok {
		writeError(w, http.StatusBadRequest, "language must be one of: python, node, bash")
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	if req.Timeout <= 0 {
		req.Timeout = cfg.DefaultTimeout
	}
	req.Timeout = min(req.Timeout, cfg.MaxTimeout)
	for k := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			writeError(w, http.StatusBadRequest, "invalid env key: "+k)
			return
		}
	}

	res, err := m.Exec(r.Context(), &req)
	if err != nil {
		if errors.Is(err, ErrTooManySandboxes) {
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		slog.Error("exec failed", "scope", req.Scope, "session", req.Session, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func handleRemove(m *Manager, w http.ResponseWriter, r *http.Request) {
	scope, session := r.PathValue("scope"), r.PathValue("session")
	if !validName(w, "scope", scope, false) || !validName(w, "session", session, true) {
		return
	}
	if err := m.Remove(scope, session, r.URL.Query().Get("purge") == "true"); err != nil {
		if errors.Is(err, ErrSandboxBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
