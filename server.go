package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Options struct {
	Registration bool  // whether /users/create makes accounts
	MaxBookBytes int64 // the largest book Athene may send
}

type Server struct {
	store  *Store
	opts   Options
	mux    *http.ServeMux
	plugin *pluginBundle
}

func NewServer(store *Store, opts Options) *Server {
	s := &Server{store: store, opts: opts, mux: http.NewServeMux(), plugin: loadPlugin()}

	// KOReader's Progress sync, as koreader/koreader-sync-server answers it.
	s.mux.HandleFunc("GET /healthcheck", s.kosync(s.healthcheck))
	s.mux.HandleFunc("POST /users/create", s.kosync(s.createUser))
	s.mux.HandleFunc("GET /users/auth", s.kosync(s.authUser))
	s.mux.HandleFunc("PUT /syncs/progress", s.kosync(s.putProgress))
	s.mux.HandleFunc("GET /syncs/progress/{document}", s.kosync(s.getProgress))

	// Full sync.
	s.mux.HandleFunc("GET /athene/v1/info", s.info)
	s.mux.HandleFunc("GET /athene/v1/devices", s.authed(s.devices))
	s.mux.HandleFunc("GET /athene/v1/annotations", s.authed(s.annotationDocuments))
	s.mux.HandleFunc("GET /athene/v1/annotations/{document}", s.authed(s.getAnnotations))
	s.mux.HandleFunc("POST /athene/v1/annotations/{document}", s.authed(s.syncAnnotations))
	s.mux.HandleFunc("GET /athene/v1/books", s.authed(s.listBooks))
	s.mux.HandleFunc("POST /athene/v1/books", s.authed(s.addBook))
	s.mux.HandleFunc("GET /athene/v1/books/{id}", s.authed(s.getBook))
	s.mux.HandleFunc("GET /athene/v1/books/{id}/file", s.authed(s.bookFile))
	s.mux.HandleFunc("POST /athene/v1/books/{id}/delivered", s.authed(s.delivered))
	s.mux.HandleFunc("DELETE /athene/v1/books/{id}", s.authed(s.removeBook))
	s.mux.HandleFunc("GET /athene/v1/plugin", s.pluginInfo)
	s.mux.HandleFunc("GET /athene/v1/plugin/files/{path...}", s.pluginFile)

	// For a person with a browser.
	s.mux.HandleFunc("GET /plugin", s.pluginZip)
	s.mux.HandleFunc("GET /athene.koplugin.zip", s.pluginZip)
	s.mux.HandleFunc("GET /source", s.source)
	s.mux.HandleFunc("GET /{$}", s.home)
	return s
}

// ServeHTTP logs each request by method, path and status: never a query (a book's title rides in
// one), a header or a body.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r)
	log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// ---- KOReader's Progress sync -----------------------------------------------------------------

var acceptPattern = regexp.MustCompile(`^application/vnd\.koreader\.v(\d+)(.*)\+json$`)

type kosyncHandler func(w http.ResponseWriter, r *http.Request, body map[string]any)

// kosync wraps a Progress sync route: KOReader's server insists on its Accept header and a JSON
// object for a body, and says so with its own error codes.
func (s *Server) kosync(h kosyncHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		if accept == "" {
			kosyncError(w, http.StatusPreconditionFailed, 100, "Accept header not specified.")
			return
		}
		m := acceptPattern.FindStringSubmatch(accept)
		if m == nil {
			kosyncError(w, http.StatusPreconditionFailed, 101, "Invalid Accept header format.")
			return
		}
		if m[1] != "1" {
			kosyncError(w, http.StatusPreconditionFailed, 102, "Unsupported API version specified.")
			return
		}
		body := map[string]any{}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			kosyncError(w, http.StatusBadRequest, 103, "Invalid JSON.")
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			var v any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&v); err != nil {
				kosyncError(w, http.StatusBadRequest, 103, "Invalid JSON.")
				return
			}
			obj, ok := v.(map[string]any)
			if !ok {
				kosyncError(w, http.StatusBadRequest, 104, "JSON body must be an object.")
				return
			}
			body = obj
		}
		h(w, r, body)
	}
}

func (s *Server) healthcheck(w http.ResponseWriter, r *http.Request, _ map[string]any) {
	writeJSONResponse(w, http.StatusOK, map[string]any{"state": "OK"})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, body map[string]any) {
	if !s.opts.Registration {
		kosyncError(w, http.StatusPaymentRequired, 2005, "User registration is disabled.")
		return
	}
	name, _ := body["username"].(string)
	key, _ := body["password"].(string)
	if name == "" || strings.Contains(name, ":") || key == "" || len(name) > 256 || len(key) > 256 {
		kosyncError(w, http.StatusForbidden, 2003, "Invalid request")
		return
	}
	if err := s.store.CreateAccount(name, key); err != nil {
		if errors.Is(err, ErrNameTaken) {
			kosyncError(w, http.StatusPaymentRequired, 2002, "Username is already registered.")
			return
		}
		log.Printf("making an account: %v", err)
		kosyncError(w, http.StatusInternalServerError, 2000, "Unknown server error.")
		return
	}
	writeJSONResponse(w, http.StatusCreated, map[string]any{"username": name})
}

func (s *Server) authUser(w http.ResponseWriter, r *http.Request, _ map[string]any) {
	if s.signedIn(r) == nil {
		kosyncError(w, http.StatusUnauthorized, 2001, "Unauthorized")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"authorized": "OK"})
}

func (s *Server) putProgress(w http.ResponseWriter, r *http.Request, body map[string]any) {
	u := s.signedIn(r)
	if u == nil {
		kosyncError(w, http.StatusUnauthorized, 2001, "Unauthorized")
		return
	}
	doc, _ := body["document"].(string)
	if doc == "" || strings.Contains(doc, ":") {
		kosyncError(w, http.StatusForbidden, 2004, "Field 'document' not provided.")
		return
	}
	if !validDocument(doc) {
		kosyncError(w, http.StatusForbidden, 2007, "Invalid document.")
		return
	}
	pct, pctOK := numberOf(body["percentage"])
	progress, progressOK := stringOf(body["progress"])
	device, deviceOK := body["device"].(string)
	if !pctOK || !progressOK || !deviceOK {
		kosyncError(w, http.StatusForbidden, 2003, "Invalid request")
		return
	}
	deviceID, _ := body["device_id"].(string)
	p := Progress{Percentage: pct, Progress: progress, Device: device, DeviceID: deviceID, Timestamp: time.Now().Unix()}
	if md, ok := body["metadata"]; ok && md != nil {
		if raw, err := json.Marshal(md); err == nil && len(raw) < 64<<10 {
			p.Metadata = raw
		}
	}
	if err := u.PutProgress(doc, p); err != nil {
		log.Printf("keeping a place: %v", err)
		kosyncError(w, http.StatusInternalServerError, 2000, "Unknown server error.")
		return
	}
	u.Seen(deviceID, device, "")
	writeJSONResponse(w, http.StatusOK, map[string]any{"document": doc, "timestamp": p.Timestamp})
}

func (s *Server) getProgress(w http.ResponseWriter, r *http.Request, _ map[string]any) {
	u := s.signedIn(r)
	if u == nil {
		kosyncError(w, http.StatusUnauthorized, 2001, "Unauthorized")
		return
	}
	doc := r.PathValue("document")
	if !validDocument(doc) {
		http.NotFound(w, r)
		return
	}
	p, ok, err := u.GetProgress(doc)
	if err != nil {
		log.Printf("reading a place: %v", err)
		kosyncError(w, http.StatusInternalServerError, 2000, "Unknown server error.")
		return
	}
	if !ok {
		writeJSONResponse(w, http.StatusOK, map[string]any{})
		return
	}
	out := map[string]any{"document": doc, "percentage": p.Percentage, "progress": p.Progress, "device": p.Device, "timestamp": p.Timestamp}
	if p.DeviceID != "" {
		out["device_id"] = p.DeviceID
	}
	if len(p.Metadata) > 0 {
		out["metadata"] = p.Metadata
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// ---- full sync ----------------------------------------------------------------------------------

type userHandler func(w http.ResponseWriter, r *http.Request, u *User)

// authed wraps a full-sync route: the account is the same as Progress sync's (x-auth-user, and
// x-auth-key carrying the MD5 of the password). A device says who it is in X-Athene-Device and
// X-Athene-Device-Id, and what it is in X-Athene-Client; names are percent-encoded UTF-8.
func (s *Server) authed(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.signedIn(r)
		if u == nil {
			kosyncError(w, http.StatusUnauthorized, 2001, "Unauthorized")
			return
		}
		name, id, client := deviceOf(r)
		u.Seen(id, name, client)
		h(w, r, u)
	}
}

func (s *Server) signedIn(r *http.Request) *User {
	name, key := r.Header.Get("x-auth-user"), r.Header.Get("x-auth-key")
	if !s.store.Authorize(name, key) {
		return nil
	}
	return s.store.User(name)
}

func deviceOf(r *http.Request) (name, id, client string) {
	dec := func(v string) string {
		if out, err := url.PathUnescape(v); err == nil {
			return out
		}
		return v
	}
	return dec(r.Header.Get("X-Athene-Device")), dec(r.Header.Get("X-Athene-Device-Id")), dec(r.Header.Get("X-Athene-Client"))
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"server":       "athene-sync",
		"version":      Version,
		"api":          APIVersion,
		"plugin":       s.plugin.Version,
		"registration": s.opts.Registration,
	})
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := u.Devices()
	if err != nil {
		serverError(w, "listing devices", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"devices": list})
}

func (s *Server) annotationDocuments(w http.ResponseWriter, r *http.Request, u *User) {
	docs, err := u.AnnotationDocuments()
	if err != nil {
		serverError(w, "listing annotated books", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"documents": docs})
}

func (s *Server) getAnnotations(w http.ResponseWriter, r *http.Request, u *User) {
	doc := r.PathValue("document")
	if !validDocument(doc) {
		apiError(w, http.StatusBadRequest, "not a book's name")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	rev, changed, err := u.SyncAnnotations(doc, since, nil, "", "")
	if err != nil {
		serverError(w, "reading annotations", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"document": doc, "rev": rev, "annotations": changed})
}

// syncAnnotations: {"since": n, "annotations": [records]} → {"rev", "annotations": changed since n}.
func (s *Server) syncAnnotations(w http.ResponseWriter, r *http.Request, u *User) {
	doc := r.PathValue("document")
	if !validDocument(doc) {
		apiError(w, http.StatusBadRequest, "not a book's name")
		return
	}
	var body struct {
		Since       int64    `json:"since"`
		Annotations []Record `json:"annotations"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(body.Annotations) > 10000 {
		apiError(w, http.StatusRequestEntityTooLarge, "too many annotations at once")
		return
	}
	for _, rec := range body.Annotations {
		if raw, err := json.Marshal(rec); err != nil || len(raw) > 256<<10 {
			apiError(w, http.StatusRequestEntityTooLarge, "an annotation is too large")
			return
		}
	}
	name, id, _ := deviceOf(r)
	rev, changed, err := u.SyncAnnotations(doc, body.Since, body.Annotations, name, id)
	if err != nil {
		serverError(w, "keeping annotations", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"document": doc, "rev": rev, "annotations": changed})
}

func (s *Server) listBooks(w http.ResponseWriter, r *http.Request, u *User) {
	books, err := u.Books()
	if err != nil {
		serverError(w, "listing books", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"books": books})
}

// addBook: the file is the body; ?filename=…&title=…&authors=… say what it is.
func (s *Server) addBook(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()
	filename := cleanFilename(q.Get("filename"))
	if filename == "" {
		apiError(w, http.StatusBadRequest, "a book needs a file name")
		return
	}
	if r.ContentLength > s.opts.MaxBookBytes {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("books can be up to %d MB on this server", s.opts.MaxBookBytes>>20))
		return
	}
	name, id, _ := deviceOf(r)
	book, fresh, err := u.AddBook(r.Body, s.opts.MaxBookBytes, filename, clip(q.Get("title"), 500), clip(q.Get("authors"), 500), name, id)
	switch {
	case errors.Is(err, errTooLarge):
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("books can be up to %d MB on this server", s.opts.MaxBookBytes>>20))
		return
	case errors.Is(err, errEmpty):
		apiError(w, http.StatusBadRequest, "the book was empty")
		return
	case err != nil:
		serverError(w, "keeping a book", err)
		return
	}
	status := http.StatusOK
	if fresh {
		status = http.StatusCreated
	}
	writeJSONResponse(w, status, map[string]any{"book": book})
}

func (s *Server) getBook(w http.ResponseWriter, r *http.Request, u *User) {
	book, _, err := u.Book(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusNotFound, "no such book")
		return
	} else if err != nil {
		serverError(w, "reading a book", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"book": book})
}

func (s *Server) bookFile(w http.ResponseWriter, r *http.Request, u *User) {
	book, path, err := u.Book(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusNotFound, "no such book")
		return
	} else if err != nil {
		serverError(w, "reading a book", err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		apiError(w, http.StatusNotFound, "the book's file is missing")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		serverError(w, "reading a book", err)
		return
	}
	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(book.Filename)))
	switch strings.ToLower(filepath.Ext(book.Filename)) {
	case ".epub":
		ctype = "application/epub+zip"
	case ".pdf":
		ctype = "application/pdf"
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(book.Filename))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (s *Server) delivered(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Device   string `json:"device"`
		DeviceID string `json:"device_id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
	name, id, _ := deviceOf(r)
	if body.DeviceID == "" {
		body.DeviceID, body.Device = id, name
	}
	if body.DeviceID == "" {
		apiError(w, http.StatusBadRequest, "which device has it?")
		return
	}
	book, err := u.Delivered(r.PathValue("id"), body.DeviceID, body.Device)
	if errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusNotFound, "no such book")
		return
	} else if err != nil {
		serverError(w, "noting a delivery", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"book": book})
}

func (s *Server) removeBook(w http.ResponseWriter, r *http.Request, u *User) {
	err := u.RemoveBook(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusNotFound, "no such book")
		return
	} else if err != nil {
		serverError(w, "removing a book", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- answers ------------------------------------------------------------------------------------

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":"server error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func kosyncError(w http.ResponseWriter, status, code int, message string) {
	writeJSONResponse(w, status, map[string]any{"code": code, "message": message})
}

func apiError(w http.ResponseWriter, status int, message string) {
	writeJSONResponse(w, status, map[string]any{"error": message})
}

func serverError(w http.ResponseWriter, doing string, err error) {
	log.Printf("%s: %v", doing, err)
	apiError(w, http.StatusInternalServerError, "the server couldn't do that; its log says why")
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

// stringOf: KOReader sends a PDF's place as a page number in a string; take a bare number too.
func stringOf(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	}
	return "", false
}

// cleanFilename makes a name a file can safely have on the server and on an e-reader's card.
func cleanFilename(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, `\`, "/")))
	if name == "." || name == "/" {
		return ""
	}
	var b strings.Builder
	for _, c := range name {
		switch {
		case c < 0x20 || c == 0x7f:
		case strings.ContainsRune(`<>:"/\|?*`, c):
			b.WriteRune('_')
		default:
			b.WriteRune(c)
		}
	}
	name = strings.Trim(b.String(), " .")
	return clip(name, 180)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
