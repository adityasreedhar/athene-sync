package main

// The data folder:
//
//	accounts.json                      each account: a salt and a hash of KOReader's key
//	users/<id>/user.json               the account's name, for whoever looks after the server
//	users/<id>/progress.json           the place in each book (KOReader's Progress sync)
//	users/<id>/devices.json            the devices that have spoken, and when
//	users/<id>/books.json              the books sent to the e-reader, and where they went
//	users/<id>/books/<book id>.<ext>   the books themselves
//	users/<id>/annotations/<doc>.json  each book's highlights, notes and bookmarks
//
// A book is known by KOReader's name for it: an MD5 of samples of its file, or of its file
// name, 32 hex digits. Every write goes to a new file that then replaces the old, so a server
// stopped halfway never leaves half a file.

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNameTaken = errors.New("that name has an account")
	ErrNotFound  = errors.New("not found")
)

// How long a deletion is remembered, so that a device that was away hears of it.
const tombstoneLifetime = 365 * 24 * time.Hour

// PBKDF2 rounds over KOReader's key. A verified key is remembered for the life of the process,
// so this is paid once per account per start, which a Raspberry Pi can afford.
const keyRounds = 60000

type Account struct {
	Salt    string `json:"salt"`
	Hash    string `json:"hash"`
	Created int64  `json:"created"`
}

type Store struct {
	dir string

	mu       sync.Mutex
	accounts map[string]*Account
	verified map[string][32]byte // name → SHA-256 of a key that passed, this run
	locks    map[string]*sync.Mutex
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "users"), 0o700); err != nil {
		return nil, fmt.Errorf("can't make the data folder %s: %w", dir, err)
	}
	s := &Store{dir: dir, accounts: map[string]*Account{}, verified: map[string][32]byte{}, locks: map[string]*sync.Mutex{}}
	var file struct {
		Accounts map[string]*Account `json:"accounts"`
	}
	if err := readJSON(filepath.Join(dir, "accounts.json"), &file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("can't read accounts.json: %w", err)
	}
	if file.Accounts != nil {
		s.accounts = file.Accounts
	}
	// A write that was cut short leaves its temporary file; the real one is intact.
	_ = os.Remove(filepath.Join(dir, "accounts.json.tmp"))
	return s, nil
}

func (s *Store) saveAccountsLocked() error {
	return writeJSON(filepath.Join(s.dir, "accounts.json"), map[string]any{"accounts": s.accounts})
}

// CreateAccount keeps a new account. `key` is what KOReader sends in place of a password: the
// MD5 of it. Only a salted, slow hash of the key is kept.
func (s *Store) CreateAccount(name, key string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash := pbkdf2SHA256([]byte(key), salt, keyRounds, 32)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.accounts[name]; taken {
		return ErrNameTaken
	}
	s.accounts[name] = &Account{Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(hash), Created: time.Now().Unix()}
	if err := s.saveAccountsLocked(); err != nil {
		delete(s.accounts, name)
		return err
	}
	u := s.userLocked(name)
	return writeJSON(filepath.Join(u.dir, "user.json"), map[string]any{"username": name, "created": time.Now().Unix()})
}

// Authorize says whether `key` is the account's.
func (s *Store) Authorize(name, key string) bool {
	if name == "" || key == "" {
		return false
	}
	sum := sha256.Sum256([]byte(key))
	s.mu.Lock()
	acct := s.accounts[name]
	known, ok := s.verified[name]
	s.mu.Unlock()
	if acct == nil {
		return false
	}
	if ok && subtle.ConstantTimeCompare(known[:], sum[:]) == 1 {
		return true
	}
	salt, err1 := hex.DecodeString(acct.Salt)
	want, err2 := hex.DecodeString(acct.Hash)
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(key), salt, keyRounds, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false
	}
	s.mu.Lock()
	s.verified[name] = sum
	s.mu.Unlock()
	return true
}

func (s *Store) AccountCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

// User is one account's corner of the data folder. Every read and write of its files happens
// under its lock, so two devices syncing at once take turns.
type User struct {
	Name string
	dir  string
	mu   *sync.Mutex
}

func (s *Store) User(name string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userLocked(name)
}

func (s *Store) userLocked(name string) *User {
	sum := sha256.Sum256([]byte("athene-sync\x00" + name))
	id := hex.EncodeToString(sum[:16])
	lock := s.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[id] = lock
	}
	dir := filepath.Join(s.dir, "users", id)
	_ = os.MkdirAll(filepath.Join(dir, "annotations"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "books"), 0o700)
	return &User{Name: name, dir: dir, mu: lock}
}

// ---- the place in each book: KOReader's Progress sync ----------------------------------------

type Progress struct {
	Percentage float64         `json:"percentage"`
	Progress   string          `json:"progress"`
	Device     string          `json:"device"`
	DeviceID   string          `json:"device_id,omitempty"`
	Timestamp  int64           `json:"timestamp"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

func (u *User) PutProgress(document string, p Progress) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	all := map[string]Progress{}
	if err := readJSON(filepath.Join(u.dir, "progress.json"), &all); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	all[document] = p
	return writeJSON(filepath.Join(u.dir, "progress.json"), all)
}

func (u *User) GetProgress(document string) (Progress, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	all := map[string]Progress{}
	if err := readJSON(filepath.Join(u.dir, "progress.json"), &all); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Progress{}, false, err
	}
	p, ok := all[document]
	return p, ok, nil
}

// ---- devices ---------------------------------------------------------------------------------

type Device struct {
	ID       string `json:"device_id"`
	Name     string `json:"device"`
	Client   string `json:"client,omitempty"`
	LastSeen int64  `json:"last_seen"`
}

// Seen notes that a device spoke. Written at most once a minute per device, unless what it calls
// itself changed.
func (u *User) Seen(id, name, client string) {
	if id == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	all := map[string]Device{}
	_ = readJSON(filepath.Join(u.dir, "devices.json"), &all)
	now := time.Now().UnixMilli()
	d, ok := all[id]
	if ok && now-d.LastSeen < 60_000 && (name == "" || d.Name == name) && (client == "" || d.Client == client) {
		return
	}
	d.ID = id
	if name != "" {
		d.Name = name
	}
	if client != "" {
		d.Client = client
	}
	d.LastSeen = now
	all[id] = d
	_ = writeJSON(filepath.Join(u.dir, "devices.json"), all)
}

func (u *User) Devices() ([]Device, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	all := map[string]Device{}
	if err := readJSON(filepath.Join(u.dir, "devices.json"), &all); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	list := make([]Device, 0, len(all))
	for _, d := range all {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].LastSeen > list[j].LastSeen })
	return list, nil
}

// ---- highlights, notes and bookmarks -----------------------------------------------------------

// A record is one highlight, note or bookmark, as the clients describe it: the server reads only
// "id", "updated" (milliseconds, the writer's clock) and "deleted", and keeps the rest as given.
// It stamps each kept change with "rev", the book's change counter, so a client can ask for
// everything since the last time it looked, and "at", the server's own clock when it took it.
type Record = map[string]any

type annotationFile struct {
	Rev     int64             `json:"rev"`
	Records map[string]Record `json:"records"`
}

func (u *User) annotationsPath(document string) string {
	return filepath.Join(u.dir, "annotations", document+".json")
}

func (u *User) loadAnnotations(document string) (*annotationFile, error) {
	f := &annotationFile{Records: map[string]Record{}}
	if err := readJSON(u.annotationsPath(document), f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if f.Records == nil {
		f.Records = map[string]Record{}
	}
	return f, nil
}

// SyncAnnotations takes the records a client sends, keeps each one that is newer than the
// server's copy, and answers with the book's change counter and every record changed since
// `since` — the client's own included, and the server's copy of any it sent that lost.
func (u *User) SyncAnnotations(document string, since int64, incoming []Record, deviceName, deviceID string) (int64, []Record, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f, err := u.loadAnnotations(document)
	if err != nil {
		return 0, nil, err
	}
	changed := false
	lost := map[string]bool{}
	for _, rec := range incoming {
		id, _ := rec["id"].(string)
		if !validRecordID(id) {
			continue
		}
		updated := int64Of(rec["updated"])
		if updated <= 0 {
			continue
		}
		clean := Record{}
		if isTrue(rec["deleted"]) {
			// A deletion keeps nothing of what was deleted.
			clean["id"], clean["updated"], clean["deleted"] = id, updated, true
		} else {
			for k, v := range rec {
				if k == "rev" || k == "at" || k == "deleted" {
					continue
				}
				clean[k] = v
			}
			clean["updated"] = updated
		}
		if _, ok := clean["device_id"]; !ok && deviceID != "" {
			clean["device_id"] = deviceID
		}
		if _, ok := clean["device"]; !ok && deviceName != "" {
			clean["device"] = deviceName
		}
		if old, ok := f.Records[id]; ok {
			oldUpdated := int64Of(old["updated"])
			if oldUpdated > updated || (oldUpdated == updated && sameRecord(old, clean)) {
				if !sameRecord(old, clean) {
					lost[id] = true
				}
				continue
			}
			if oldUpdated == updated {
				// The same moment, said two ways: the server's stands, the client takes it.
				lost[id] = true
				continue
			}
		}
		f.Rev++
		clean["rev"] = f.Rev
		clean["at"] = time.Now().UnixMilli() // when the server took it: a deletion is aged by this, not by a device's clock
		f.Records[id] = clean
		changed = true
	}
	if pruneTombstones(f) {
		changed = true
	}
	if changed {
		if err := writeJSON(u.annotationsPath(document), f); err != nil {
			return 0, nil, err
		}
	}
	out := make([]Record, 0)
	for id, rec := range f.Records {
		if int64Of(rec["rev"]) > since || lost[id] {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return int64Of(out[i]["rev"]) < int64Of(out[j]["rev"]) })
	return f.Rev, out, nil
}

// AnnotationDocuments lists the books that have annotations kept, with their change counters.
func (u *User) AnnotationDocuments() (map[string]int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(u.dir, "annotations"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]int64{}, nil
		}
		return nil, err
	}
	out := map[string]int64{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		doc := strings.TrimSuffix(name, ".json")
		if !validDocument(doc) {
			continue
		}
		var f annotationFile
		if readJSON(filepath.Join(u.dir, "annotations", name), &f) == nil {
			out[doc] = f.Rev
		}
	}
	return out, nil
}

func pruneTombstones(f *annotationFile) bool {
	cutoff := time.Now().Add(-tombstoneLifetime).UnixMilli()
	pruned := false
	for id, rec := range f.Records {
		at := int64Of(rec["at"])
		if at == 0 {
			at = int64Of(rec["updated"])
		}
		if isTrue(rec["deleted"]) && at < cutoff {
			delete(f.Records, id)
			pruned = true
		}
	}
	return pruned
}

func sameRecord(a, b Record) bool {
	strip := func(r Record) Record {
		c := Record{}
		for k, v := range r {
			if k != "rev" && k != "at" {
				c[k] = v
			}
		}
		return c
	}
	ja, err1 := json.Marshal(strip(a))
	jb, err2 := json.Marshal(strip(b))
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

// ---- books -----------------------------------------------------------------------------------

type Delivery struct {
	DeviceID string `json:"device_id"`
	Device   string `json:"device"`
	At       int64  `json:"at"`
}

type Book struct {
	ID         string     `json:"id"`
	Document   string     `json:"document"`
	Filename   string     `json:"filename"`
	Title      string     `json:"title,omitempty"`
	Authors    string     `json:"authors,omitempty"`
	Size       int64      `json:"size"`
	Added      int64      `json:"added"`
	FromDevice string     `json:"from_device,omitempty"`
	FromID     string     `json:"from_device_id,omitempty"`
	Deliveries []Delivery `json:"deliveries"`
}

func (b *Book) storedName() string {
	ext := strings.ToLower(filepath.Ext(b.Filename))
	if len(ext) > 10 || strings.ContainsAny(ext, `/\`) {
		ext = ""
	}
	return b.ID + ext
}

func (u *User) loadBooks() ([]*Book, error) {
	var f struct {
		Books []*Book `json:"books"`
	}
	if err := readJSON(filepath.Join(u.dir, "books.json"), &f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, b := range f.Books {
		if b.Deliveries == nil {
			b.Deliveries = []Delivery{}
		}
	}
	return f.Books, nil
}

func (u *User) saveBooks(books []*Book) error {
	if books == nil {
		books = []*Book{}
	}
	return writeJSON(filepath.Join(u.dir, "books.json"), map[string]any{"books": books})
}

func (u *User) Books() ([]*Book, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	books, err := u.loadBooks()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(books, func(i, j int) bool { return books[i].Added > books[j].Added })
	if books == nil {
		books = []*Book{}
	}
	return books, nil
}

func (u *User) Book(id string) (*Book, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	books, err := u.loadBooks()
	if err != nil {
		return nil, "", err
	}
	for _, b := range books {
		if b.ID == id {
			return b, filepath.Join(u.dir, "books", b.storedName()), nil
		}
	}
	return nil, "", ErrNotFound
}

// AddBook keeps a book from `body`, up to `limit` bytes. The same file sent again (KOReader's name
// for it and its size) is the same book: its title and file name are brought up to date, and no
// second copy is kept. `fresh` says whether it was new.
func (u *User) AddBook(body io.Reader, limit int64, filename, title, authors, fromDevice, fromID string) (book *Book, fresh bool, err error) {
	tmp, err := os.CreateTemp(filepath.Join(u.dir, "books"), "upload-*.part")
	if err != nil {
		return nil, false, err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	size, err := io.Copy(tmp, io.LimitReader(body, limit+1))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, false, err
	}
	if size > limit {
		return nil, false, errTooLarge
	}
	if size == 0 {
		return nil, false, errEmpty
	}
	document, err := partialMD5(tmpName)
	if err != nil {
		return nil, false, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	books, err := u.loadBooks()
	if err != nil {
		return nil, false, err
	}
	for _, b := range books {
		if b.Document == document && b.Size == size {
			if filename != "" && filename != b.Filename {
				// Keep the file under its new name's extension too.
				oldPath := filepath.Join(u.dir, "books", b.storedName())
				b.Filename = filename
				_ = os.Rename(oldPath, filepath.Join(u.dir, "books", b.storedName()))
			}
			if title != "" {
				b.Title = title
			}
			if authors != "" {
				b.Authors = authors
			}
			return b, false, u.saveBooks(books)
		}
	}
	book = &Book{
		ID: randomHex(8), Document: document, Filename: filename, Title: title, Authors: authors,
		Size: size, Added: time.Now().UnixMilli(), FromDevice: fromDevice, FromID: fromID, Deliveries: []Delivery{},
	}
	if err := os.Rename(tmpName, filepath.Join(u.dir, "books", book.storedName())); err != nil {
		return nil, false, err
	}
	tmpName = ""
	books = append(books, book)
	if err := u.saveBooks(books); err != nil {
		return nil, false, err
	}
	return book, true, nil
}

// Delivered notes that a device has the book.
func (u *User) Delivered(id, deviceID, deviceName string) (*Book, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	books, err := u.loadBooks()
	if err != nil {
		return nil, err
	}
	for _, b := range books {
		if b.ID != id {
			continue
		}
		now := time.Now().UnixMilli()
		for i := range b.Deliveries {
			if b.Deliveries[i].DeviceID == deviceID {
				b.Deliveries[i].At = now
				if deviceName != "" {
					b.Deliveries[i].Device = deviceName
				}
				return b, u.saveBooks(books)
			}
		}
		b.Deliveries = append(b.Deliveries, Delivery{DeviceID: deviceID, Device: deviceName, At: now})
		return b, u.saveBooks(books)
	}
	return nil, ErrNotFound
}

// RemoveBook deletes a book and its file. Its annotations stay: they belong to the book, not to
// this copy of it.
func (u *User) RemoveBook(id string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	books, err := u.loadBooks()
	if err != nil {
		return err
	}
	for i, b := range books {
		if b.ID == id {
			_ = os.Remove(filepath.Join(u.dir, "books", b.storedName()))
			books = append(books[:i], books[i+1:]...)
			return u.saveBooks(books)
		}
	}
	return ErrNotFound
}

var (
	errTooLarge = errors.New("book too large")
	errEmpty    = errors.New("empty book")
)

// ---- helpers ---------------------------------------------------------------------------------

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// writeJSON replaces a file whole: written beside it, flushed, then renamed over it.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func isTrue(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func validRecordID(id string) bool {
	if len(id) == 0 || len(id) > 100 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// validDocument: KOReader's names for books are MD5s, but its sync server takes any name of
// letters, digits and underscores, and so does this one.
func validDocument(doc string) bool {
	if len(doc) == 0 || len(doc) > 128 {
		return false
	}
	for _, c := range doc {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// pbkdf2SHA256 is PBKDF2 (RFC 8018) with HMAC-SHA-256.
func pbkdf2SHA256(password, salt []byte, rounds, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	u := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0], buf[1], buf[2], buf[3] = byte(block>>24), byte(block>>16), byte(block>>8), byte(block)
		prf.Write(buf)
		out = prf.Sum(out)
		t := out[len(out)-hashLen:]
		copy(u, t)
		for n := 2; n <= rounds; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range u {
				t[i] ^= u[i]
			}
		}
	}
	return out[:keyLen]
}
