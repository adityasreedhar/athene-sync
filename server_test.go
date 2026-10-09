package main

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const koAccept = "application/vnd.koreader.v1+json"

type harness struct {
	t   *testing.T
	srv *httptest.Server
	dir string
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if opts.MaxBookBytes == 0 {
		opts.MaxBookBytes = 8 << 20
	}
	srv := httptest.NewServer(NewServer(store, opts))
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, dir: dir}
}

// do sends a request; headers alternate name, value.
func (h *harness) do(method, path string, body io.Reader, headers ...string) (int, map[string]any, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		_ = dec.Decode(&out)
	}
	return resp.StatusCode, out, raw
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func (h *harness) register(name, key string) {
	h.t.Helper()
	status, out, _ := h.do("POST", "/users/create", jsonBody(map[string]any{"username": name, "password": key}), "Accept", koAccept)
	if status != 201 || out["username"] != name {
		h.t.Fatalf("register: %d %v", status, out)
	}
}

func auth(name, key string, more ...string) []string {
	return append([]string{"x-auth-user", name, "x-auth-key", key, "Accept", koAccept}, more...)
}

func code(out map[string]any) string {
	if c, ok := out["code"].(json.Number); ok {
		return c.String()
	}
	return ""
}

func TestKOSyncContract(t *testing.T) {
	h := newHarness(t, Options{Registration: true})
	key := "5f4dcc3b5aa765d61d8327deb882cf99"

	if status, out, _ := h.do("GET", "/healthcheck", nil); status != 412 || code(out) != "100" {
		t.Fatalf("no Accept: %d %v", status, out)
	}
	if status, out, _ := h.do("GET", "/healthcheck", nil, "Accept", "application/json"); status != 412 || code(out) != "101" {
		t.Fatalf("bad Accept: %d %v", status, out)
	}
	if status, out, _ := h.do("GET", "/healthcheck", nil, "Accept", "application/vnd.koreader.v2+json"); status != 412 || code(out) != "102" {
		t.Fatalf("v2: %d %v", status, out)
	}
	if status, out, _ := h.do("GET", "/healthcheck", nil, "Accept", koAccept); status != 200 || out["state"] != "OK" {
		t.Fatalf("healthcheck: %d %v", status, out)
	}

	h.register("reader", key)
	if status, out, _ := h.do("POST", "/users/create", jsonBody(map[string]any{"username": "reader", "password": key}), "Accept", koAccept); status != 402 || code(out) != "2002" {
		t.Fatalf("taken: %d %v", status, out)
	}
	if status, out, _ := h.do("POST", "/users/create", jsonBody(map[string]any{"username": "a:b", "password": key}), "Accept", koAccept); status != 403 || code(out) != "2003" {
		t.Fatalf("colon: %d %v", status, out)
	}
	if status, out, _ := h.do("GET", "/users/auth", nil, auth("reader", key)...); status != 200 || out["authorized"] != "OK" {
		t.Fatalf("auth: %d %v", status, out)
	}
	if status, out, _ := h.do("GET", "/users/auth", nil, auth("reader", "nope")...); status != 401 || code(out) != "2001" {
		t.Fatalf("wrong key: %d %v", status, out)
	}

	doc := "0123456789abcdef0123456789abcdef"
	if status, out, _ := h.do("GET", "/syncs/progress/"+doc, nil, auth("reader", key)...); status != 200 || len(out) != 0 {
		t.Fatalf("nothing yet: %d %v", status, out)
	}
	put := map[string]any{"document": doc, "progress": "/body/DocFragment[3]/body/p[2]/text().4", "percentage": 0.25, "device": "Kobo_io", "device_id": "K1"}
	status, out, _ := h.do("PUT", "/syncs/progress", jsonBody(put), auth("reader", key)...)
	if status != 200 || out["document"] != doc || out["timestamp"] == nil {
		t.Fatalf("put: %d %v", status, out)
	}
	status, out, _ = h.do("GET", "/syncs/progress/"+doc, nil, auth("reader", key)...)
	if status != 200 || out["progress"] != put["progress"] || out["device"] != "Kobo_io" || out["device_id"] != "K1" || out["percentage"].(json.Number).String() != "0.25" {
		t.Fatalf("get: %d %v", status, out)
	}
	if status, out, _ := h.do("PUT", "/syncs/progress", jsonBody(map[string]any{"document": "bad-doc!", "progress": "1", "percentage": 0.1, "device": "x"}), auth("reader", key)...); status != 403 || code(out) != "2007" {
		t.Fatalf("bad document: %d %v", status, out)
	}
	if status, out, _ := h.do("PUT", "/syncs/progress", jsonBody(map[string]any{"document": doc, "progress": "1", "device": "x"}), auth("reader", key)...); status != 403 || code(out) != "2003" {
		t.Fatalf("no percentage: %d %v", status, out)
	}
	if status, out, _ := h.do("PUT", "/syncs/progress", jsonBody(map[string]any{"progress": "1", "percentage": 0.1, "device": "x"}), auth("reader", key)...); status != 403 || code(out) != "2004" {
		t.Fatalf("no document: %d %v", status, out)
	}
}

func TestRegistrationClosed(t *testing.T) {
	h := newHarness(t, Options{Registration: false})
	status, out, _ := h.do("POST", "/users/create", jsonBody(map[string]any{"username": "r", "password": "k"}), "Accept", koAccept)
	if status != 402 || code(out) != "2005" {
		t.Fatalf("closed: %d %v", status, out)
	}
	status, out, _ = h.do("GET", "/athene/v1/info", nil)
	if status != 200 || out["server"] != "athene-sync" || out["registration"] != false {
		t.Fatalf("info: %d %v", status, out)
	}
}

func TestAccountsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir)
	if err := store.CreateAccount("reader", "key1"); err != nil {
		t.Fatal(err)
	}
	again, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Authorize("reader", "key1") || again.Authorize("reader", "key2") {
		t.Fatal("reopened store doesn't know the account")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if bytes.Contains(raw, []byte("key1")) {
		t.Fatal("the key is kept as it came")
	}
}

func records(out map[string]any) map[string]map[string]any {
	got := map[string]map[string]any{}
	list, _ := out["annotations"].([]any)
	for _, item := range list {
		rec := item.(map[string]any)
		got[rec["id"].(string)] = rec
	}
	return got
}

func TestAnnotations(t *testing.T) {
	h := newHarness(t, Options{Registration: true})
	h.register("reader", "k")
	doc := "aaaabbbbccccddddeeeeffff00001111"
	path := "/athene/v1/annotations/" + doc
	kobo := auth("reader", "k", "X-Athene-Device", "Kobo_io", "X-Athene-Device-Id", "K1", "X-Athene-Client", "koreader-plugin/1.0.0")
	phone := auth("reader", "k", "X-Athene-Device", "Athene%20(iPhone)", "X-Athene-Device-Id", "P1")

	hl := map[string]any{"id": "H1", "updated": 1000, "kind": "highlight", "pos0": "/body/DocFragment[2]/body/p[1]/text().0",
		"pos1": "/body/DocFragment[2]/body/p[1]/text().9", "text": "It was the", "color": "yellow", "drawer": "lighten"}
	status, out, _ := h.do("POST", path, jsonBody(map[string]any{"since": 0, "annotations": []any{hl}}), kobo...)
	if status != 200 || out["rev"].(json.Number).String() != "1" {
		t.Fatalf("first push: %d %v", status, out)
	}
	got := records(out)
	if got["H1"]["device"] != "Kobo_io" || got["H1"]["device_id"] != "K1" || got["H1"]["rev"].(json.Number).String() != "1" {
		t.Fatalf("first push kept: %v", got["H1"])
	}

	// The phone pulls it, then pushes an older edit, which loses: the server's copy comes back.
	status, out, _ = h.do("GET", path+"?since=0", nil, phone...)
	if status != 200 || len(records(out)) != 1 {
		t.Fatalf("pull: %d %v", status, out)
	}
	older := map[string]any{"id": "H1", "updated": 900, "kind": "highlight", "note": "older"}
	status, out, _ = h.do("POST", path, jsonBody(map[string]any{"since": 1, "annotations": []any{older}}), phone...)
	got = records(out)
	if status != 200 || out["rev"].(json.Number).String() != "1" || got["H1"] == nil || got["H1"]["note"] != nil {
		t.Fatalf("older edit should lose and come back: %d %v", status, out)
	}

	// A newer edit wins.
	newer := map[string]any{"id": "H1", "updated": 2000, "kind": "highlight", "note": "a note", "text": "It was the"}
	status, out, _ = h.do("POST", path, jsonBody(map[string]any{"since": 1, "annotations": []any{newer}}), phone...)
	got = records(out)
	if status != 200 || out["rev"].(json.Number).String() != "2" || got["H1"]["note"] != "a note" || got["H1"]["device"] != "Athene (iPhone)" {
		t.Fatalf("newer edit: %d %v", status, out)
	}

	// The same record again changes nothing.
	status, out, _ = h.do("POST", path, jsonBody(map[string]any{"since": 2, "annotations": []any{newer}}), phone...)
	if status != 200 || out["rev"].(json.Number).String() != "2" || len(records(out)) != 0 {
		t.Fatalf("echo: %d %v", status, out)
	}

	// A deletion keeps nothing of what was deleted, and reaches a device that asks since before.
	gone := map[string]any{"id": "H1", "updated": 3000, "deleted": true, "text": "It was the", "note": "a note"}
	status, out, _ = h.do("POST", path, jsonBody(map[string]any{"since": 2, "annotations": []any{gone}}), kobo...)
	got = records(out)
	if status != 200 || got["H1"]["deleted"] != true || got["H1"]["text"] != nil || got["H1"]["note"] != nil {
		t.Fatalf("deletion: %d %v", status, out)
	}
	status, out, _ = h.do("GET", path+"?since=2", nil, phone...)
	if got = records(out); status != 200 || len(got) != 1 || got["H1"]["deleted"] != true {
		t.Fatalf("deletion pulled: %d %v", status, out)
	}

	// Bad ids are skipped; the documents list knows the book.
	status, _, _ = h.do("POST", path, jsonBody(map[string]any{"since": 3, "annotations": []any{map[string]any{"id": "../x", "updated": 5}}}), phone...)
	if status != 200 {
		t.Fatalf("bad id: %d", status)
	}
	status, out, _ = h.do("GET", "/athene/v1/annotations", nil, phone...)
	docs, _ := out["documents"].(map[string]any)
	if status != 200 || docs[doc] == nil {
		t.Fatalf("documents: %d %v", status, out)
	}
	if status, _, _ := h.do("GET", "/athene/v1/annotations/not!a!doc", nil, phone...); status != 400 {
		t.Fatalf("bad document: %d", status)
	}

	// Devices are remembered by what they said they were.
	status, out, _ = h.do("GET", "/athene/v1/devices", nil, phone...)
	devs, _ := out["devices"].([]any)
	if status != 200 || len(devs) != 2 {
		t.Fatalf("devices: %d %v", status, out)
	}
	if status, _, _ := h.do("GET", "/athene/v1/devices", nil, "x-auth-user", "reader", "x-auth-key", "wrong"); status != 401 {
		t.Fatalf("devices without the key: %d", status)
	}
}

func TestBooks(t *testing.T) {
	h := newHarness(t, Options{Registration: true, MaxBookBytes: 1 << 20})
	h.register("reader", "k")
	phone := auth("reader", "k", "X-Athene-Device", "Athene%20(iPhone)", "X-Athene-Device-Id", "P1")
	kobo := auth("reader", "k", "X-Athene-Device", "Kobo_io", "X-Athene-Device-Id", "K1")

	file := bytes.Repeat([]byte("Call me Ishmael. "), 4000) // 68 KB
	q := "?filename=Moby%20Dick%20-%20Herman%20Melville.epub&title=Moby%20Dick&authors=Herman%20Melville"
	status, out, _ := h.do("POST", "/athene/v1/books"+q, bytes.NewReader(file), phone...)
	if status != 201 {
		t.Fatalf("upload: %d %v", status, out)
	}
	book := out["book"].(map[string]any)
	id := book["id"].(string)
	if book["document"] != referencePartialMD5(file) || book["filename"] != "Moby Dick - Herman Melville.epub" || book["from_device"] != "Athene (iPhone)" {
		t.Fatalf("book: %v", book)
	}

	// The same file again is the same book.
	status, out, _ = h.do("POST", "/athene/v1/books"+q, bytes.NewReader(file), phone...)
	if status != 200 || out["book"].(map[string]any)["id"] != id {
		t.Fatalf("again: %d %v", status, out)
	}

	status, out, _ = h.do("GET", "/athene/v1/books", nil, kobo...)
	if list, _ := out["books"].([]any); status != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", status, out)
	}
	status, _, raw := h.do("GET", "/athene/v1/books/"+id+"/file", nil, kobo...)
	if status != 200 || !bytes.Equal(raw, file) {
		t.Fatalf("download: %d, %d bytes", status, len(raw))
	}
	status, out, _ = h.do("POST", "/athene/v1/books/"+id+"/delivered", jsonBody(map[string]any{}), kobo...)
	deliveries, _ := out["book"].(map[string]any)["deliveries"].([]any)
	if status != 200 || len(deliveries) != 1 || deliveries[0].(map[string]any)["device_id"] != "K1" {
		t.Fatalf("delivered: %d %v", status, out)
	}

	if status, _, _ := h.do("POST", "/athene/v1/books?filename=big.pdf", bytes.NewReader(make([]byte, 2<<20)), phone...); status != 413 {
		t.Fatalf("too large: %d", status)
	}
	if status, _, _ := h.do("POST", "/athene/v1/books?filename=", bytes.NewReader(file), phone...); status != 400 {
		t.Fatalf("no name: %d", status)
	}
	if status, _, _ := h.do("DELETE", "/athene/v1/books/"+id, nil, phone...); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	if status, _, _ := h.do("GET", "/athene/v1/books/"+id+"/file", nil, kobo...); status != 404 {
		t.Fatalf("deleted file: %d", status)
	}
	left, _ := filepath.Glob(filepath.Join(h.dir, "users", "*", "books", "*"))
	if len(left) != 0 {
		t.Fatalf("files left behind: %v", left)
	}
}

func TestPluginAndSource(t *testing.T) {
	h := newHarness(t, Options{Registration: true})
	status, out, _ := h.do("GET", "/athene/v1/plugin", nil)
	files, _ := out["files"].([]any)
	if status != 200 || out["version"] == "unknown" || len(files) < 2 {
		t.Fatalf("plugin: %d %v", status, out)
	}
	status, _, raw := h.do("GET", "/plugin", nil)
	if status != 200 {
		t.Fatalf("zip: %d", status)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["athene.koplugin/_meta.lua"] || !names["athene.koplugin/main.lua"] {
		t.Fatalf("zip holds %v", names)
	}
	status, _, raw = h.do("GET", "/athene/v1/plugin/files/_meta.lua", nil)
	if status != 200 || !bytes.Contains(raw, []byte("version")) {
		t.Fatalf("plugin file: %d", status)
	}
	if status, _, _ := h.do("GET", "/athene/v1/plugin/files/../go.mod", nil); status == 200 {
		t.Fatal("served a file outside the plugin")
	}
	status, _, raw = h.do("GET", "/source", nil)
	if status != 200 || len(raw) < 1000 || raw[0] != 0x1f {
		t.Fatalf("source: %d, %d bytes", status, len(raw))
	}
	status, _, raw = h.do("GET", "/", nil)
	if status != 200 || !bytes.Contains(raw, []byte("Athene sync server")) {
		t.Fatalf("home: %d", status)
	}
}

// referencePartialMD5 is KOReader's util.partialMD5 written out again, for a file held in memory.
func referencePartialMD5(b []byte) string {
	h := md5.New()
	for _, off := range []int{0, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456, 1073741824} {
		if off >= len(b) {
			break
		}
		end := off + 1024
		if end > len(b) {
			end = len(b)
		}
		h.Write(b[off:end])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestPartialMD5(t *testing.T) {
	dir := t.TempDir()
	for _, size := range []int{1, 1000, 1024, 5000, 70000, 300000} {
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(i * 7 % 251)
		}
		p := filepath.Join(dir, fmt.Sprint(size))
		_ = os.WriteFile(p, b, 0o600)
		got, err := partialMD5(p)
		if err != nil || got != referencePartialMD5(b) {
			t.Fatalf("size %d: %s %v", size, got, err)
		}
	}
}
