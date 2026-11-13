//go:build windows

// erp-executor is a thin "hands and eyes" HTTP service for driving an ERP
// RemoteApp window from the RDP client machine. It knows nothing about the ERP:
// the orchestrator decides what to do; this service only looks and types.
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type config struct {
	addr        string
	title       string
	token       string
	allowFrom   []*net.IPNet
	allowedKeys map[string]bool // nil = any key allowed
	allowType   bool
	allowClick  bool
	typeMode    string
	keyDelay    time.Duration
	charDelay   time.Duration
	maxText     int
	auditDir    string
	auditDelay  time.Duration
	tlsCert     string
	tlsKey      string
}

type server struct {
	cfg     config
	mu      sync.Mutex // serializes everything that touches the UI
	paused  atomic.Bool
	auditMu sync.Mutex
}

// ---------- errors & responses ----------

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return &apiError{code: code, msg: fmt.Sprintf(format, a...)}
}

func progress(err error, done, total int) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return &apiError{code: ae.code, msg: fmt.Sprintf("%s (stopped after %d of %d steps)", ae.msg, done, total)}
	}
	return fail(http.StatusInternalServerError, "%v (stopped after %d of %d steps)", err, done, total)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fail(http.StatusBadRequest, "invalid JSON body: %v", err)
	}
	return nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *server) route(h handler) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w := &statusRecorder{ResponseWriter: rw, status: http.StatusOK}
		defer func() {
			log.Printf("%s %s %s -> %d (%s)", r.RemoteAddr, r.Method, r.URL.Path,
				w.status, time.Since(start).Round(time.Millisecond))
		}()
		if !s.clientAllowed(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "client address not allowed"})
			return
		}
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "missing or bad bearer token"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := h(w, r); err != nil {
			code := http.StatusInternalServerError
			var ae *apiError
			if errors.As(err, &ae) {
				code = ae.code
			}
			writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		}
	}
}

func (s *server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.token)) == 1
}

func (s *server) clientAllowed(r *http.Request) bool {
	if len(s.cfg.allowFrom) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	for _, n := range s.cfg.allowFrom {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------- locating the ERP ----------

type target struct {
	Main    Window   `json:"main"`
	Related []Window `json:"dialogs"` // other visible windows of the same RemoteApp process
}

func (t target) bounds() Rect {
	b := t.Main.Rect
	for _, w := range t.Related {
		b = b.Union(w.Rect)
	}
	return b
}

func (t target) contains(x, y int) bool {
	if t.Main.Rect.Contains(x, y) {
		return true
	}
	for _, w := range t.Related {
		if w.Rect.Contains(x, y) {
			return true
		}
	}
	return false
}

// findTarget picks the main ERP window by title, then collects the other
// windows from the same mstsc process. In RemoteApp, each ERP dialog shows up
// as its own local window, so this is how dialogs are detected.
func (s *server) findTarget() (target, error) {
	needle := strings.ToLower(s.cfg.title)
	hwnds := topLevelWindows()
	var best *Window
	for _, h := range hwnds {
		if !isVisible(h) {
			continue
		}
		w := describeWindow(h)
		if !strings.Contains(strings.ToLower(w.Title), needle) {
			continue
		}
		if best == nil || (w.Minimized != best.Minimized && !w.Minimized) ||
			(w.Minimized == best.Minimized && w.Rect.Area() > best.Rect.Area()) {
			wc := w
			best = &wc
		}
	}
	if best == nil {
		return target{}, fail(http.StatusNotFound, "no visible window whose title contains %q", s.cfg.title)
	}
	t := target{Main: *best}
	for _, h := range hwnds {
		if h == best.HWND || !isVisible(h) || windowPID(h) != best.PID {
			continue
		}
		w := describeWindow(h)
		if w.Minimized || w.Rect.W <= 0 || w.Rect.H <= 0 {
			continue
		}
		t.Related = append(t.Related, w)
	}
	return t, nil
}

func (s *server) failsafeTripped() bool {
	p, ok := cursorPos()
	if !ok {
		return false
	}
	vs := virtualScreen()
	if int(p.X) <= vs.X && int(p.Y) <= vs.Y {
		if !s.paused.Swap(true) {
			log.Printf("FAILSAFE: cursor in top-left corner, executor paused. POST /resume to continue.")
		}
		return true
	}
	return false
}

// prepare runs every precondition check before input and focuses the ERP.
func (s *server) prepare() (target, error) {
	if s.paused.Load() {
		return target{}, fail(http.StatusLocked, "executor is paused; POST /resume to continue")
	}
	if !inputDesktopAvailable() {
		return target{}, fail(http.StatusServiceUnavailable,
			"input desktop unavailable (workstation locked, or a UAC / secure desktop is showing)")
	}
	if s.failsafeTripped() {
		return target{}, fail(http.StatusLocked, "failsafe: mouse cursor is in the top-left screen corner; executor paused")
	}
	t, err := s.findTarget()
	if err != nil {
		return t, err
	}
	if err := focusWindow(t.Main.HWND, t.Main.PID); err != nil {
		return t, fail(http.StatusConflict, "%v", err)
	}
	return s.findTarget() // re-read: restoring a window changes its rect
}

// guard is checked between every keystroke/chord, so a human can pause the bot
// and so input stops if anything else steals focus mid-sequence.
func (s *server) guard(pid uint32) func() error {
	return func() error {
		if s.paused.Load() {
			return fail(http.StatusLocked, "paused mid-sequence")
		}
		if s.failsafeTripped() {
			return fail(http.StatusLocked, "failsafe tripped mid-sequence")
		}
		if windowPID(foregroundWindow()) != pid {
			return fail(http.StatusConflict, "ERP window lost focus mid-sequence")
		}
		return nil
	}
}

func (s *server) parseKeys(keys []string) ([]chord, error) {
	if len(keys) == 0 {
		return nil, fail(http.StatusBadRequest, "keys must not be empty")
	}
	if len(keys) > 200 {
		return nil, fail(http.StatusBadRequest, "too many keys in one request (max 200)")
	}
	out := make([]chord, 0, len(keys))
	for _, k := range keys {
		c, err := parseChord(k)
		if err != nil {
			return nil, fail(http.StatusBadRequest, "%v", err)
		}
		if s.cfg.allowedKeys != nil && !s.cfg.allowedKeys[c.String()] {
			return nil, fail(http.StatusForbidden, "key %q is not in the allowlist", c.String())
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *server) sendChords(chords []chord, delay time.Duration, guard func() error) error {
	for i, c := range chords {
		if err := guard(); err != nil {
			return progress(err, i, len(chords))
		}
		if err := sendKeyboardSeq(c.events()); err != nil {
			return progress(err, i, len(chords))
		}
		time.Sleep(delay)
	}
	return nil
}

// ---------- handlers ----------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) error {
	desk := inputDesktopAvailable()
	paused := s.paused.Load()
	resp := map[string]any{"paused": paused, "input_desktop": desk}
	ok := desk && !paused
	if t, err := s.findTarget(); err != nil {
		ok = false
		resp["error"] = err.Error()
	} else {
		resp["main"] = t.Main
		resp["dialogs"] = len(t.Related)
		resp["foreground_is_erp"] = windowPID(foregroundWindow()) == t.Main.PID
		if t.Main.Minimized {
			ok = false
			resp["error"] = "ERP window is minimized"
		}
	}
	resp["ok"] = ok
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
	return nil
}

func (s *server) handleWindows(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Query().Get("all") == "1" {
		out := []Window{}
		for _, h := range topLevelWindows() {
			if !isVisible(h) {
				continue
			}
			if win := describeWindow(h); win.Title != "" {
				out = append(out, win)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "windows": out})
		return nil
	}
	t, err := s.findTarget()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "main": t.Main, "dialogs": t.Related})
	return nil
}

func (s *server) handleFocus(w http.ResponseWriter, r *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.prepare()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "main": t.Main, "dialogs": t.Related})
	return nil
}

// GET /screenshot?scope=window|all|screen&focus=0|1
func (s *server) handleScreenshot(w http.ResponseWriter, r *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	if !inputDesktopAvailable() {
		return fail(http.StatusServiceUnavailable, "input desktop unavailable (locked?); a capture would be blank")
	}
	var t target
	var err error
	if q.Get("focus") == "0" || s.paused.Load() {
		t, err = s.findTarget()
	} else {
		t, err = s.prepare()
	}
	if err != nil {
		return err
	}

	scope := q.Get("scope")
	var rc Rect
	switch scope {
	case "", "window":
		rc = t.Main.Rect
	case "all":
		rc = t.bounds()
	case "screen":
		rc = virtualScreen()
	default:
		return fail(http.StatusBadRequest, "scope must be window, all or screen")
	}
	if t.Main.Minimized && scope != "screen" {
		return fail(http.StatusConflict, "ERP window is minimized")
	}

	img, err := captureRect(rc)
	if err != nil {
		return fail(http.StatusInternalServerError, "%v", err)
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Origin", fmt.Sprintf("%d,%d", rc.X, rc.Y))
	m := t.Main.Rect
	h.Set("X-Main-Rect", fmt.Sprintf("%d,%d,%d,%d", m.X, m.Y, m.W, m.H))
	h.Set("X-Dialogs", fmt.Sprint(len(t.Related)))
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(buf.Bytes())
	return err
}

// POST /keys {"keys": ["alt+f", "n", "tab"], "delay_ms": 60}
func (s *server) handleKeys(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Keys    []string `json:"keys"`
		DelayMS *int     `json:"delay_ms"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	chords, err := s.parseKeys(req.Keys)
	if err != nil {
		return err
	}
	delay := s.cfg.keyDelay
	if req.DelayMS != nil {
		delay = time.Duration(minInt(maxInt(*req.DelayMS, 0), 10000)) * time.Millisecond
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.prepare()
	if err != nil {
		return err
	}
	if err := s.sendChords(chords, delay, s.guard(t.Main.PID)); err != nil {
		s.audit("keys-failed", err.Error(), t.bounds())
		return err
	}
	names := make([]string, len(chords))
	for i, c := range chords {
		names[i] = c.String()
	}
	s.audit("keys", strings.Join(names, " "), t.bounds())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": len(chords)})
	return nil
}

// POST /type {"text": "ACME-1042"}
func (s *server) handleType(w http.ResponseWriter, r *http.Request) error {
	if !s.cfg.allowType {
		return fail(http.StatusForbidden, "typing is disabled (-allow-type=false)")
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	n := utf8.RuneCountInString(req.Text)
	if n == 0 {
		return fail(http.StatusBadRequest, "text must not be empty")
	}
	if n > s.cfg.maxText {
		return fail(http.StatusRequestEntityTooLarge, "text is %d chars; max is %d", n, s.cfg.maxText)
	}
	if s.cfg.typeMode == "keys" && capsLockOn() {
		return fail(http.StatusConflict, "Caps Lock is on; turn it off (or run with -type-mode unicode)")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.prepare()
	if err != nil {
		return err
	}
	if err := typeText(req.Text, s.cfg.typeMode == "unicode", s.cfg.charDelay, s.guard(t.Main.PID)); err != nil {
		s.audit("type-failed", err.Error(), t.bounds())
		return err
	}
	s.audit("type", fmt.Sprintf("%d chars", n), t.bounds()) // never log the text itself
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "typed": n})
	return nil
}

// POST /click {"x": 120, "y": 340, "space": "window"|"screen", "button": "left", "double": false}
func (s *server) handleClick(w http.ResponseWriter, r *http.Request) error {
	if !s.cfg.allowClick {
		return fail(http.StatusForbidden, "clicking is disabled (-allow-click=false)")
	}
	var req struct {
		X      int    `json:"x"`
		Y      int    `json:"y"`
		Space  string `json:"space"`
		Button string `json:"button"`
		Double bool   `json:"double"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Button == "" {
		req.Button = "left"
	}
	if req.Button != "left" && req.Button != "right" && req.Button != "middle" {
		return fail(http.StatusBadRequest, "button must be left, right or middle")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.prepare()
	if err != nil {
		return err
	}
	sx, sy := req.X, req.Y
	switch req.Space {
	case "", "window":
		sx += t.Main.Rect.X
		sy += t.Main.Rect.Y
	case "screen":
	default:
		return fail(http.StatusBadRequest, "space must be window or screen")
	}
	if !t.contains(sx, sy) {
		return fail(http.StatusBadRequest, "point (%d,%d) is outside the ERP windows", sx, sy)
	}
	if vs := virtualScreen(); sx <= vs.X && sy <= vs.Y {
		return fail(http.StatusBadRequest, "refusing to click the failsafe corner")
	}
	if err := clickAt(sx, sy, req.Button, req.Double); err != nil {
		return fail(http.StatusInternalServerError, "%v", err)
	}
	s.audit("click", fmt.Sprintf("%s %d,%d double=%v", req.Button, sx, sy, req.Double), t.bounds())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "screen": map[string]int{"x": sx, "y": sy}})
	return nil
}

func (s *server) handleGetClipboard(w http.ResponseWriter, r *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, err := readClipboard()
	if err != nil {
		return fail(http.StatusConflict, "%v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": text})
	return nil
}

// POST /clipboard {"text": "..."} — set it, then send ctrl+v via /keys to paste.
func (s *server) handleSetClipboard(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeClipboard(req.Text); err != nil {
		return fail(http.StatusConflict, "%v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// POST /copy {"keys": ["ctrl+c"], "timeout_ms": 3000}
// Sends the keys, waits for the redirected clipboard to change, returns the text.
func (s *server) handleCopy(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Keys      []string `json:"keys"`
		TimeoutMS int      `json:"timeout_ms"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if len(req.Keys) == 0 {
		req.Keys = []string{"ctrl+c"}
	}
	if req.TimeoutMS == 0 {
		req.TimeoutMS = 3000
	}
	timeout := time.Duration(minInt(maxInt(req.TimeoutMS, 100), 15000)) * time.Millisecond
	chords, err := s.parseKeys(req.Keys)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.prepare()
	if err != nil {
		return err
	}
	before := clipboardSeq()
	if err := s.sendChords(chords, s.cfg.keyDelay, s.guard(t.Main.PID)); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for clipboardSeq() == before {
		if time.Now().After(deadline) {
			return fail(http.StatusGatewayTimeout,
				"clipboard did not change within %s (nothing selected, or clipboard redirection disabled for this RDP connection?)", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // let the RDP clipboard channel settle
	text, err := readClipboard()
	if err != nil {
		return fail(http.StatusConflict, "%v", err)
	}
	s.audit("copy", fmt.Sprintf("%d chars", utf8.RuneCountInString(text)), t.bounds())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": text})
	return nil
}

func (s *server) handlePause(w http.ResponseWriter, r *http.Request) error {
	s.paused.Store(true)
	log.Printf("paused via API")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": true})
	return nil
}

func (s *server) handleResume(w http.ResponseWriter, r *http.Request) error {
	s.paused.Store(false)
	log.Printf("resumed via API")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": false})
	return nil
}

// ---------- audit trail ----------

// audit saves a screenshot shortly after each action plus a JSON line in
// audit.log. Typed text is never written, only its length.
func (s *server) audit(action, detail string, area Rect) {
	if s.cfg.auditDir == "" {
		return
	}
	go func() {
		time.Sleep(s.cfg.auditDelay)
		now := time.Now()
		entry := map[string]any{"time": now.Format(time.RFC3339Nano), "action": action, "detail": detail}
		if img, err := captureRect(area); err == nil {
			name := fmt.Sprintf("%s_%s.png", now.Format("20060102-150405.000"), action)
			if f, err := os.Create(filepath.Join(s.cfg.auditDir, name)); err == nil {
				_ = png.Encode(f, img)
				f.Close()
				entry["screenshot"] = name
			}
		} else {
			entry["screenshot_error"] = err.Error()
		}
		line, _ := json.Marshal(entry)

		s.auditMu.Lock()
		defer s.auditMu.Unlock()
		f, err := os.OpenFile(filepath.Join(s.cfg.auditDir, "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			log.Printf("audit: %v", err)
			return
		}
		defer f.Close()
		_, _ = f.Write(append(line, '\n'))
	}()
}

// ---------- main ----------

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	var cfg config
	var allowKeys, allowFrom string
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:8765", "listen address; use the LAN IP only if the orchestrator runs on another machine")
	flag.StringVar(&cfg.title, "title", "", "case-insensitive substring of the ERP main window title (required)")
	flag.StringVar(&allowFrom, "allow-from", "", "comma-separated client IPs/CIDRs allowed to connect (empty = any, token still required)")
	flag.StringVar(&allowKeys, "allow-keys", "", `comma-separated allowlist of key chords, e.g. "tab,enter,alt+f,ctrl+c,f8" (empty = any)`)
	flag.BoolVar(&cfg.allowType, "allow-type", true, "allow POST /type")
	flag.BoolVar(&cfg.allowClick, "allow-click", true, "allow POST /click")
	flag.StringVar(&cfg.typeMode, "type-mode", "keys", "keys = physical key presses via client layout; unicode = Unicode key events")
	flag.DurationVar(&cfg.keyDelay, "key-delay", 60*time.Millisecond, "pause after each key chord")
	flag.DurationVar(&cfg.charDelay, "char-delay", 15*time.Millisecond, "pause after each typed character")
	flag.IntVar(&cfg.maxText, "max-text", 2000, "max characters per /type call")
	flag.StringVar(&cfg.auditDir, "audit-dir", "", "if set, save a screenshot + log line after every action here")
	flag.DurationVar(&cfg.auditDelay, "audit-delay", 300*time.Millisecond, "wait before the audit screenshot")
	flag.StringVar(&cfg.tlsCert, "tls-cert", "", "TLS certificate file (enables HTTPS)")
	flag.StringVar(&cfg.tlsKey, "tls-key", "", "TLS key file")
	flag.Parse()

	cfg.token = os.Getenv("EXECUTOR_TOKEN")
	if cfg.title == "" {
		log.Fatal("-title is required")
	}
	if len(cfg.token) < 16 {
		log.Fatal("set the EXECUTOR_TOKEN environment variable to a random secret (16+ chars)")
	}
	if cfg.typeMode != "keys" && cfg.typeMode != "unicode" {
		log.Fatal("-type-mode must be keys or unicode")
	}
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		log.Fatal("-tls-cert and -tls-key must be set together")
	}
	for _, c := range splitList(allowFrom) {
		if !strings.Contains(c, "/") {
			if ip := net.ParseIP(c); ip != nil && ip.To4() != nil {
				c += "/32"
			} else {
				c += "/128"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			log.Fatalf("-allow-from: %v", err)
		}
		cfg.allowFrom = append(cfg.allowFrom, n)
	}
	if keys := splitList(allowKeys); len(keys) > 0 {
		cfg.allowedKeys = map[string]bool{}
		for _, k := range keys {
			c, err := parseChord(k)
			if err != nil {
				log.Fatalf("-allow-keys: %v", err)
			}
			cfg.allowedKeys[c.String()] = true
		}
	}
	if cfg.auditDir != "" {
		if err := os.MkdirAll(cfg.auditDir, 0o700); err != nil {
			log.Fatalf("-audit-dir: %v", err)
		}
	}

	enableDPIAwareness()
	s := &server{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.route(s.handleHealth))
	mux.HandleFunc("GET /windows", s.route(s.handleWindows))
	mux.HandleFunc("POST /focus", s.route(s.handleFocus))
	mux.HandleFunc("GET /screenshot", s.route(s.handleScreenshot))
	mux.HandleFunc("POST /keys", s.route(s.handleKeys))
	mux.HandleFunc("POST /waitfor", s.route(s.handleWaitFor))
	mux.HandleFunc("POST /type", s.route(s.handleType))
	mux.HandleFunc("POST /click", s.route(s.handleClick))
	mux.HandleFunc("GET /clipboard", s.route(s.handleGetClipboard))
	mux.HandleFunc("POST /clipboard", s.route(s.handleSetClipboard))
	mux.HandleFunc("POST /copy", s.route(s.handleCopy))
	mux.HandleFunc("POST /pause", s.route(s.handlePause))
	mux.HandleFunc("POST /resume", s.route(s.handleResume))

	host, _, _ := net.SplitHostPort(cfg.addr)
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback {
		log.Printf("WARNING: listening on %s (not loopback). Firewall it to the orchestrator host and/or use -allow-from.", cfg.addr)
		if cfg.tlsCert == "" {
			log.Printf("WARNING: no TLS; the bearer token crosses the network in cleartext.")
		}
	}
	if t, err := s.findTarget(); err != nil {
		log.Printf("ERP window not found yet: %v", err)
	} else {
		log.Printf("found ERP window %q (pid %d) at %+v", t.Main.Title, t.Main.PID, t.Main.Rect)
	}
	log.Printf("failsafe: slam the mouse into the top-left screen corner to pause all input")

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
	}
	log.Printf("erp-executor listening on %s", cfg.addr)
	if cfg.tlsCert != "" {
		log.Fatal(srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey))
	}
	log.Fatal(srv.ListenAndServe())
}