package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

//go:embed web
var webFS embed.FS

const sourceURL = "https://educationboardresults.gov.bd/v2/home"

var (
	validBoards = map[string]bool{"barisal": true, "chittagong": true, "comilla": true, "dhaka": true,
		"dinajpur": true, "jessore": true, "madrasah": true, "mymensingh": true, "rajshahi": true,
		"sylhet": true, "tec": true}
	digits = regexp.MustCompile(`^\d{1,15}$`)
)

type Subject struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
	Grade   string `json:"grade"`
}

type Result struct {
	Info     map[string]string `json:"info"`
	Subjects []Subject         `json:"subjects"`
	Text     string            `json:"-"`
}

// Runs inside the result page (written locally; no remote scripts).
const extractJS = `(() => {
  const info = {}, subjects = [];
  document.querySelectorAll('tr').forEach(tr => {
    const c = [...tr.children].map(x => x.innerText.trim());
    if (c.length === 3 && /^\d+$/.test(c[0])) subjects.push({code: c[0], subject: c[1], grade: c[2]});
    else if (c.length >= 2 && c.length % 2 === 0)
      for (let i = 0; i < c.length; i += 2) if (c[i] && c[i+1]) info[c[i]] = c[i+1];
  });
  return JSON.stringify({info, subjects, Text: document.body.innerText.slice(0, 300)});
})()`

// Tags the captcha image so we can screenshot it.
const tagCaptchaJS = `(() => {
  const c = document.getElementById('captcha'); let img = null;
  for (let p = c && c.parentElement; p && !img; p = p.parentElement) img = p.querySelector('img');
  if (!img) return false; img.id = 'cimg'; return true;
})()`

type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	created time.Time
}

var (
	mu       sync.Mutex
	sessions = map[string]*session{}
	lastSeen = map[string]time.Time{}
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { writeJSON(w, code, map[string]string{"error": msg}) }

func newSession() *session {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", os.Getenv("SHOW_BROWSER") != "1"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-sandbox", os.Getenv("NO_SANDBOX") == "1"),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"),
	)
	a, c1 := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, c2 := chromedp.NewContext(a)
	ctx, c3 := context.WithTimeout(ctx, 4*time.Minute)
	return &session{ctx: ctx, created: time.Now(), cancel: func() { c3(); c2(); c1() }}
}


// setField sets a form field by id using page JS, retrying until it exists and accepts the value
// (the board site redraws parts of the form, which breaks node-based actions).
func setField(ctx context.Context, id, val string) error {
	js := fmt.Sprintf(`(()=>{const e=document.getElementById(%q);if(!e)return false;e.value=%q;
e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}));return e.value===%q;})()`, id, val, val)
	for i := 0; i < 30; i++ {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(js, &ok)); err == nil && ok {
			return chromedp.Run(ctx, chromedp.Sleep(400*time.Millisecond))
		}
		chromedp.Run(ctx, chromedp.Sleep(500*time.Millisecond))
	}
	return fmt.Errorf("field #%s not found or value %q not accepted", id, val)
}

func clickID(ctx context.Context, id string) error {
	var ok bool
	js := fmt.Sprintf(`(()=>{const e=document.getElementById(%q);if(!e)return false;e.click();return true;})()`, id)
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &ok)); err != nil || !ok {
		return fmt.Errorf("button #%s not found", id)
	}
	return nil
}

func captchaShot(ctx context.Context) (string, error) {
	var ok bool
	var buf []byte
	err := chromedp.Run(ctx,
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Evaluate(tagCaptchaJS, &ok),
	)
	if err != nil || !ok {
		return "", fmt.Errorf("could not find the captcha image on the board site")
	}
	var serr error
	for i := 0; i < 3; i++ {
		if serr = chromedp.Run(ctx, chromedp.Screenshot("#cimg", &buf, chromedp.ByID)); serr == nil {
			break
		}
		chromedp.Run(ctx, chromedp.Sleep(700*time.Millisecond), chromedp.Evaluate(tagCaptchaJS, &ok))
	}
	if serr != nil {
		return "", serr
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf), nil
}

func startHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	board, year, roll, reg := strings.ToLower(q.Get("board")), q.Get("year"), q.Get("roll"), q.Get("reg")
	y, _ := strconv.Atoi(year)
	switch {
	case !validBoards[board]:
		fail(w, 400, "Choose a valid board.")
		return
	case y < 1996 || y > time.Now().Year():
		fail(w, 400, "Year must be between 1996 and now.")
		return
	case !digits.MatchString(roll) || !digits.MatchString(reg):
		fail(w, 400, "Roll and registration must be numbers.")
		return
	}
	ip := clientIP(r)
	mu.Lock()
	if time.Since(lastSeen[ip]) < 10*time.Second {
		mu.Unlock()
		fail(w, 429, "Please wait 10 seconds between searches.")
		return
	}
	if len(sessions) >= maxSessions() {
		mu.Unlock()
		fail(w, 503, "Server is busy. Try again shortly.")
		return
	}
	lastSeen[ip] = time.Now()
	s := newSession()
	b := make([]byte, 12)
	rand.Read(b)
	id := hex.EncodeToString(b)
	sessions[id] = s
	mu.Unlock()

	err := chromedp.Run(s.ctx, chromedp.Navigate(sourceURL), chromedp.Sleep(2*time.Second))
	for _, kv := range [][2]string{{"board", board}, {"exam", "hsc"}, {"year", year},
		{"result_type", "1"}, {"roll", roll}, {"reg", reg}} {
		if err != nil {
			break
		}
		err = setField(s.ctx, kv[0], kv[1])
	}
	var img string
	if err == nil {
		img, err = captchaShot(s.ctx)
	}
	if err != nil {
		closeSession(id)
		log.Println("start error:", err)
		fail(w, 502, "Could not open the board site: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": id, "captcha": img})
}

// clientIP returns the visitor IP; behind a proxy (Render, Nginx...) set TRUST_PROXY=1.
func clientIP(r *http.Request) string {
	if os.Getenv("TRUST_PROXY") == "1" {
		if x := r.Header.Get("X-Forwarded-For"); x != "" {
			return strings.TrimSpace(strings.Split(x, ",")[0])
		}
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func maxSessions() int {
	if n, err := strconv.Atoi(os.Getenv("MAX_SESSIONS")); err == nil && n > 0 {
		return n
	}
	return 3
}

func getSession(id string) *session {
	mu.Lock()
	defer mu.Unlock()
	return sessions[id]
}

func closeSession(id string) {
	mu.Lock()
	s := sessions[id]
	delete(sessions, id)
	mu.Unlock()
	if s != nil {
		s.cancel()
	}
}

func reloadHandler(w http.ResponseWriter, r *http.Request) {
	s := getSession(r.URL.Query().Get("id"))
	if s == nil {
		fail(w, 410, "Session expired. Start again.")
		return
	}
	if err := clickID(s.ctx, "captcha_reload"); err != nil {
		fail(w, 502, err.Error())
		return
	}
	img, err := captchaShot(s.ctx)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"captcha": img})
}

func submitHandler(w http.ResponseWriter, r *http.Request) {
	id, code := r.URL.Query().Get("id"), r.URL.Query().Get("captcha")
	s := getSession(id)
	if s == nil {
		fail(w, 410, "Session expired. Start again.")
		return
	}
	defer closeSession(id)
	if !digits.MatchString(code) {
		fail(w, 400, "Captcha must be a number.")
		return
	}
	if err := setField(s.ctx, "captcha", code); err != nil {
		fail(w, 502, err.Error())
		return
	}
	if err := clickID(s.ctx, "submit"); err != nil {
		fail(w, 502, err.Error())
		return
	}
	var res Result
	for i := 0; i < 20; i++ {
		var raw string
		chromedp.Run(s.ctx, chromedp.Sleep(1*time.Second), chromedp.Evaluate(extractJS, &raw))
		json.Unmarshal([]byte(raw), &res)
		if len(res.Subjects) > 0 {
			writeJSON(w, 200, res)
			return
		}
	}
	msg := "No result found. The captcha may be wrong, or the roll/registration/board/year doesn't match."
	if t := strings.TrimSpace(res.Text); t != "" {
		log.Println("page text:", t)
	}
	fail(w, 404, msg)
}

func main() {
	go func() {
		for range time.Tick(30 * time.Second) {
			mu.Lock()
			for id, s := range sessions {
				if time.Since(s.created) > 4*time.Minute {
					s.cancel()
					delete(sessions, id)
				}
			}
			mu.Unlock()
		}
	}()
	sub, _ := fs.Sub(webFS, "web")
	http.Handle("/", http.FileServer(http.FS(sub)))
	http.HandleFunc("/api/start", startHandler)
	http.HandleFunc("/api/reload", reloadHandler)
	http.HandleFunc("/api/submit", submitHandler)
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	log.Println("listening on http://localhost" + addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
