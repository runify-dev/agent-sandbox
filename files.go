package main

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// 工作区里的文件都是沙箱用户写的，可能包含指向宿主机任意位置的符号链接。
// 所有文件操作都通过 os.Root 进行，路径无法逃出工作区（包括 ../ 和符号链接）。

type fileEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

func registerFileRoutes(mux *http.ServeMux, cfg *Config, m *Manager) {
	mux.Handle("PUT /v1/files/{scope}/{path...}", requireKey(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleUpload(cfg, m, w, r)
	})))
	mux.Handle("GET /v1/files/{scope}/{path...}", requireKey(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleDownload(m, w, r)
	})))
	mux.Handle("DELETE /v1/files/{scope}/{path...}", requireKey(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteFile(m, w, r)
	})))
}

// fileTarget 是文件接口操作的目录和其中的相对路径
type fileTarget struct {
	Scope   string
	Session string // 为空时操作 scope 的长期空间，否则操作这个对话的目录
	Rel     string // 目录内的相对路径，根目录为 "."
}

// parseFileTarget 解析 /v1/files/{scope}/{path...}?session=<session>
func parseFileTarget(w http.ResponseWriter, r *http.Request) (t fileTarget, ok bool) {
	t.Scope, t.Session = r.PathValue("scope"), r.URL.Query().Get("session")
	if !validName(w, "scope", t.Scope, false) || !validName(w, "session", t.Session, true) {
		return t, false
	}
	p := r.PathValue("path")
	if strings.ContainsRune(p, 0) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return t, false
	}
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	if rel == "" {
		rel = "."
	}
	t.Rel = filepath.FromSlash(rel)
	return t, true
}

func (m *Manager) openWorkspace(t fileTarget) (*os.Root, error) {
	dir, err := m.workspaceDir(t.Scope, t.Session)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

// handleUpload 以请求体原始字节流写入文件，先写临时文件再改名，避免读到写了一半的文件
func handleUpload(cfg *Config, m *Manager, w http.ResponseWriter, r *http.Request) {
	t, ok := parseFileTarget(w, r)
	if !ok {
		return
	}
	rel := t.Rel
	if rel == "." {
		writeError(w, http.StatusBadRequest, "file path is required")
		return
	}
	root, err := m.openWorkspace(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer root.Close()

	dir := filepath.Dir(rel)
	if err := m.mkdirAllOwned(root, dir); err != nil {
		writeError(w, http.StatusBadRequest, "create directory: "+err.Error())
		return
	}
	tmp := filepath.Join(dir, ".upload-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		writeError(w, http.StatusBadRequest, "create file: "+err.Error())
		return
	}
	body := http.MaxBytesReader(w, r.Body, cfg.MaxUploadBytes)
	n, copyErr := f.ReadFrom(body)
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = root.Remove(tmp)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "file too large")
			return
		}
		writeError(w, http.StatusInternalServerError, "write file: "+err.Error())
		return
	}
	if err := root.Chown(tmp, m.cfg.UID, m.cfg.GID); err != nil {
		slog.Warn("chown uploaded file failed", "scope", t.Scope, "session", t.Session, "path", rel, "error", err)
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		writeError(w, http.StatusBadRequest, "save file: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": filepath.ToSlash(rel), "size": n})
}

// handleDownload 路径是文件时返回文件内容（支持 Range），是目录时返回文件列表
func handleDownload(m *Manager, w http.ResponseWriter, r *http.Request) {
	t, ok := parseFileTarget(w, r)
	if !ok {
		return
	}
	rel := t.Rel
	root, err := m.openWorkspace(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer root.Close()

	// O_NONBLOCK：防止沙箱用户放一个 FIFO 让服务在 open 时一直阻塞
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusBadRequest, "open: "+err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch {
	case fi.IsDir():
		names, err := f.Readdirnames(-1)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		entries := make([]fileEntry, 0, len(names))
		for _, name := range names {
			info, err := root.Lstat(filepath.Join(rel, name))
			if err != nil {
				continue
			}
			entries = append(entries, fileEntry{Name: name, IsDir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime()})
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": filepath.ToSlash(rel), "entries": entries})
	case fi.Mode().IsRegular():
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(fi.Name()))
		http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
	default:
		writeError(w, http.StatusBadRequest, "not a regular file")
	}
}

func handleDeleteFile(m *Manager, w http.ResponseWriter, r *http.Request) {
	t, ok := parseFileTarget(w, r)
	if !ok {
		return
	}
	rel := t.Rel
	if rel == "." {
		writeError(w, http.StatusBadRequest, "cannot delete workspace root")
		return
	}
	root, err := m.openWorkspace(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer root.Close()
	// 符号链接只删除链接本身，不会删除它指向的目标
	if err := root.RemoveAll(rel); err != nil {
		writeError(w, http.StatusBadRequest, "delete: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// mkdirAllOwned 逐级创建目录，新建的目录 chown 给沙箱用户，保证容器内可写
func (m *Manager) mkdirAllOwned(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		cur = filepath.Join(cur, part)
		err := root.Mkdir(cur, 0o755)
		if err == nil {
			if err := root.Chown(cur, m.cfg.UID, m.cfg.GID); err != nil {
				slog.Warn("chown directory failed", "path", cur, "error", err)
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}
