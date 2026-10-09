package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The KOReader plugin travels inside the server, so the e-reader always gets the one that
// matches it: as a zip for a person to copy onto the e-reader once, and file by file for the
// plugin to bring itself up to date.
//
//go:embed all:plugin
var pluginFS embed.FS

// The server's own source, served at /source: it is free software under the AGPL, which asks
// that anyone using it over a network can have it.
//
//go:embed *.go go.mod LICENSE README.md Dockerfile all:plugin
var sourceFS embed.FS

const pluginRoot = "plugin/athene.koplugin"

// A fixed date for everything zipped and tarred, so the same build serves the same bytes.
var bundleDate = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

type pluginFile struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
	data   []byte
}

type pluginBundle struct {
	Version string
	Files   []*pluginFile
	byPath  map[string]*pluginFile
	zip     []byte
}

var metaVersion = regexp.MustCompile(`version\s*=\s*"([^"]+)"`)

func loadPlugin() *pluginBundle {
	b := &pluginBundle{Version: "unknown", byPath: map[string]*pluginFile{}}
	err := fs.WalkDir(pluginFS, pluginRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := pluginFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		f := &pluginFile{Path: strings.TrimPrefix(p, pluginRoot+"/"), Size: len(data), SHA256: hex.EncodeToString(sum[:]), data: data}
		b.Files = append(b.Files, f)
		b.byPath[f.Path] = f
		return nil
	})
	if err != nil {
		log.Printf("reading the plugin: %v", err)
	}
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	if meta := b.byPath["_meta.lua"]; meta != nil {
		if m := metaVersion.FindSubmatch(meta.data); m != nil {
			b.Version = string(m[1])
		}
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range b.Files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "athene.koplugin/" + f.Path, Method: zip.Deflate, Modified: bundleDate})
		if err == nil {
			_, err = w.Write(f.data)
		}
		if err != nil {
			log.Printf("zipping the plugin: %v", err)
		}
	}
	_ = zw.Close()
	b.zip = buf.Bytes()
	return b
}

func (s *Server) pluginInfo(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{"version": s.plugin.Version, "files": s.plugin.Files})
}

func (s *Server) pluginFile(w http.ResponseWriter, r *http.Request) {
	f := s.plugin.byPath[r.PathValue("path")]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", bundleDate, bytes.NewReader(f.data))
}

func (s *Server) pluginZip(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="athene.koplugin.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", bundleDate, bytes.NewReader(s.plugin.zip))
}

func (s *Server) source(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	prefix := "athene-sync-" + Version + "/"
	err := fs.WalkDir(sourceFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := sourceFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: prefix + p, Mode: 0o644, Size: int64(len(data)), ModTime: bundleDate, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err != nil {
		serverError(w, "packing the source", err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="athene-sync-%s-source.tar.gz"`, Version))
	_, _ = w.Write(buf.Bytes())
}

// home is the page a person sees opening the server's address in a browser.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	address := "http://" + r.Host
	page := fmt.Sprintf(homePage, html.EscapeString(Version), html.EscapeString(address), html.EscapeString(s.plugin.Version), html.EscapeString(address))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}

const homePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Athene sync server</title>
<style>
:root { color-scheme: light dark; --ink: #1d1d1f; --sub: #6e6e73; --tint: #0b7c63; --card: #fff; --ground: #f4f5f7; --edge: rgba(0,0,0,.08); }
@media (prefers-color-scheme: dark) { :root { --ink: #f2f3f7; --sub: #a1a1a8; --tint: #3fd9b1; --card: #1f2026; --ground: #131418; --edge: rgba(255,255,255,.1); } }
body { margin: 0; padding: 32px 16px; background: var(--ground); color: var(--ink); font: 16px/1.5 -apple-system, system-ui, "Segoe UI", Roboto, sans-serif; }
main { max-width: 620px; margin: 0 auto; }
h1 { font-size: 26px; margin: 0 0 4px; } .sub { color: var(--sub); margin: 0 0 24px; }
.dot { display: inline-block; width: 9px; height: 9px; border-radius: 50%%; background: #34c759; margin-right: 8px; vertical-align: 1px; }
ol { background: var(--card); border: 1px solid var(--edge); border-radius: 14px; padding: 18px 18px 18px 40px; margin: 0 0 20px; }
li { margin: 6px 0; } code { font: 14px ui-monospace, Menlo, monospace; background: var(--ground); padding: 1px 5px; border-radius: 5px; }
a { color: var(--tint); font-weight: 600; } footer { color: var(--sub); font-size: 13px; }
</style></head>
<body><main>
<h1><span class="dot"></span>Your Athene sync server is running</h1>
<p class="sub">Version %s, at <code>%s</code></p>
<p>New to KOReader? <a href="https://github.com/koreader/koreader#installation">Install it on your e-reader first</a>.</p>
<ol>
<li><a href="/plugin">Download the KOReader plugin</a> and unzip it. Plug in your e-reader and copy the <code>athene.koplugin</code> folder into KOReader's <code>plugins</code> folder: on a Kobo <code>.adds/koreader/plugins</code>, on a Kindle or Boox <code>koreader/plugins</code>, on a PocketBook <code>applications/koreader/plugins</code>. A Mac hides folders whose names start with a dot: press ⌘⇧. in Finder to see them. Eject the e-reader, then restart KOReader: tap the top of the screen and choose <b>☰ ▸ Exit ▸ Restart KOReader</b>; on a Boox or another Android e-reader, choose <b>☰ ▸ Exit</b> and open KOReader again. Plugin version %s.</li>
<li>In KOReader, open a book and tap the top of the page for its menu. On the <b>Tools</b> tab (the wrench), choose <b>Progress sync ▸ Custom sync server</b>. The box starts with https://: replace it all with <code>%s</code> and tap <b>OK</b>. Then choose <b>Register / Login</b>. The first time, pick a name and password and tap <b>Register</b>.</li>
<li>So it syncs by itself on a Kobo or Kindle: on the <b>Settings</b> tab (the gear), choose <b>Network</b>, set <b>Action when Wi-Fi is off</b> to <b>Turn on</b> (KOReader won't let the next tick on until then), and tick <b>Restore Wi-Fi connection on resume</b> so it syncs as it wakes. Then tick <b>Tools ▸ Progress sync ▸ Automatically keep documents in sync</b>.</li>
<li>In Athene, open <b>Settings ▸ KOReader</b>, turn on <b>Sync with KOReader</b> and sign in with the same address, name and password. To send a book, press and hold it in your Library and choose <b>Send to KOReader</b>. In KOReader, Athene's own menu, with <b>Sync now</b>, is on the <b>Tools</b> tab, right under Progress sync.</li>
</ol>
<footer>Free software under the GNU AGPL, version 3. <a href="/source">Source code</a>.</footer>
</main></body></html>
`
