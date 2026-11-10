package ingest

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/egoist/tinytraffic/internal/store"
)

func TestParseReferrer(t *testing.T) {
	cases := []struct{ ref, page, referrer, source string }{
		{"https://www.google.co.uk/", "example.com", "google.co.uk", "Google"},
		{"https://news.ycombinator.com/item?id=1", "example.com", "news.ycombinator.com/item", "Hacker News"},
		{"https://m.facebook.com/", "example.com", "m.facebook.com", "Facebook"},
		{"https://gemini.google.com/app", "example.com", "gemini.google.com/app", "Gemini"},
		{"https://Blog.Someone.dev/posts/tools/?token=secret#top", "example.com", "blog.someone.dev/posts/tools", "blog.someone.dev"},
		{"https://t.co/AbC123", "example.com", "t.co/AbC123", "Twitter"},
		{"https://www.example.com/other", "example.com", "", ""},
		{"https://example.com/other", "www.Example.com", "", ""},
		{"android-app://com.google.android.gm", "example.com", "com.google.android.gm", "Gmail"},
		{"", "example.com", "", ""},
	}
	for _, c := range cases {
		referrer, source := parseReferrer(c.ref, c.page)
		if referrer != c.referrer || source != c.source {
			t.Errorf("parseReferrer(%q) = %q, %q; want %q, %q", c.ref, referrer, source, c.referrer, c.source)
		}
	}
}

const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"

func TestHandler(t *testing.T) {
	st, err := store.Open("", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	site, _ := st.CreateSite("example.com", "UTC")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := store.NewWriter(st.Events, log)
	h := NewHandler(Handler{Store: st, Writer: w, Salts: NewSalts(st), Log: log})

	send := func(ua, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(body))
		req.Header.Set("User-Agent", ua)
		req.RemoteAddr = "203.0.113.7:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	page := `{"n":"pageview","u":"https://www.example.com/docs?utm_source=hn","d":"example.com","r":"https://news.ycombinator.com/item?id=42","w":390}`
	if code := send(chromeUA, page); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	// No referrer or UTM tags (an SPA navigation): keeps the session's source.
	send(chromeUA, `{"n":"Signup","u":"https://example.com/docs","d":"example.com","p":{"plan":"pro","seats":3}}`)
	send("curl/8.4.0", page)                                                    // bot
	send(chromeUA, `{"n":"pageview","u":"https://other.com/","d":"other.com"}`) // unknown site
	if code := send(chromeUA, `{"n":"pageview","u":"javascript:alert(1)"}`); code != http.StatusBadRequest {
		t.Errorf("bad url: status %d", code)
	}
	w.Close()

	rows, err := st.DB.Query(`SELECT name, pathname, hostname, referrer, referrer_source, device, props, visitor_id, session_id FROM events WHERE site_id = ? ORDER BY ts`, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		name, path, host, referrer, source, device, props string
		visitor, session                                  uint64
	}
	var got []row
	for rows.Next() {
		var r row
		rows.Scan(&r.name, &r.path, &r.host, &r.referrer, &r.source, &r.device, &r.props, &r.visitor, &r.session)
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].path != "/docs" || got[0].host != "example.com" || got[0].referrer != "news.ycombinator.com/item" || got[0].source != "Hacker News" || got[0].device != "Mobile" {
		t.Errorf("pageview: %+v", got[0])
	}
	if got[1].source != "Hacker News" || got[1].referrer != "news.ycombinator.com/item" {
		t.Errorf("later events should keep the session's referrer and source, got %q, %q", got[1].referrer, got[1].source)
	}
	if got[1].props != `{"plan":"pro","seats":"3"}` {
		t.Errorf("props: %s", got[1].props)
	}
	if got[0].visitor != got[1].visitor || got[0].session != got[1].session {
		t.Errorf("events of one visit should share visitor and session ids")
	}
}

// A trusted request (e.g. a server with a write key) may forward the
// visitor's IP; anyone else's X-Forwarded-For is ignored.
func TestTrustRequestForwardedIP(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		st, err := store.Open("", store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		site, _ := st.CreateSite("example.com", "UTC")
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		w := store.NewWriter(st.Events, log)
		h := NewHandler(Handler{Store: st, Writer: w, Salts: NewSalts(st), Log: log,
			TrustRequest: func(*http.Request) bool { return trusted }})
		for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
			req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(`{"n":"pageview","u":"https://example.com/","d":"example.com"}`))
			req.Header.Set("User-Agent", chromeUA)
			req.Header.Set("X-Forwarded-For", ip)
			req.RemoteAddr = "203.0.113.9:1234" // the sending server
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
		w.Close()
		var visitors int
		st.DB.QueryRow(`SELECT count(DISTINCT visitor_id) FROM events WHERE site_id = ?`, site.ID).Scan(&visitors)
		want := 1
		if trusted {
			want = 2
		}
		if visitors != want {
			t.Errorf("trusted=%v: %d visitors, want %d", trusted, visitors, want)
		}
		st.Close()
	}
}

// Behind a proxy that sets X-Client-IP (Amp Boxes), it wins over a
// CF-Connecting-IP the visitor sent themselves.
func TestClientIPPrefersXClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("CF-Connecting-IP", "7.7.7.7")
	req.Header.Set("X-Client-IP", "198.51.100.1")
	if got := ClientIP(req, true).String(); got != "198.51.100.1" {
		t.Errorf("trusted: got %s, want 198.51.100.1", got)
	}
	if got := ClientIP(req, false).String(); got != "127.0.0.1" {
		t.Errorf("untrusted: got %s, want 127.0.0.1", got)
	}
}

// After a rename, snippets still sending the old domain keep recording, and
// an event for both domains is recorded once.
func TestRenamedSite(t *testing.T) {
	st, err := store.Open("", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	site, _ := st.CreateSite("old.com", "UTC")
	if _, err := st.UpdateSite(site.ID, store.SiteUpdate{Domain: "new.com"}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := store.NewWriter(st.Events, log)
	h := NewHandler(Handler{Store: st, Writer: w, Salts: NewSalts(st), Log: log})
	for _, d := range []string{"old.com", "new.com", "old.com,new.com"} {
		req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(`{"n":"pageview","u":"https://new.com/","d":"`+d+`"}`))
		req.Header.Set("User-Agent", chromeUA)
		req.RemoteAddr = "203.0.113.7:1234"
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	w.Close()
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM events WHERE site_id = ?`, site.ID).Scan(&n)
	if n != 3 {
		t.Errorf("%d events, want 3", n)
	}
}
