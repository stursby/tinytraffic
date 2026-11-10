// Package ingest turns tracker requests into stored events.
package ingest

import (
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"
	"github.com/mileusna/useragent"

	"github.com/egoist/tinytraffic/internal/store"
)

type Handler struct {
	Store      *store.Store
	Writer     *store.Writer
	Salts      *Salts
	Geo        *Geo
	TrustProxy bool
	// TrustRequest, when set, can vouch for a single request's proxy
	// headers, e.g. a server sending events with a write API key.
	TrustRequest func(*http.Request) bool
	Log          *slog.Logger

	sessions *sessions
}

func NewHandler(h Handler) *Handler {
	h.sessions = newSessions()
	return &h
}

// payload accepts the short keys sent by our tracker and the long ones of
// Plausible's events API, so existing integrations work as-is.
type payload struct {
	N           string          `json:"n"`
	Name        string          `json:"name"`
	U           string          `json:"u"`
	URL         string          `json:"url"`
	D           string          `json:"d"`
	Domain      string          `json:"domain"`
	R           string          `json:"r"`
	Referrer    string          `json:"referrer"`
	W           int             `json:"w"`
	ScreenWidth int             `json:"screen_width"`
	H           int             `json:"h"`
	P           json.RawMessage `json:"p"`
	Props       json.RawMessage `json:"props"`
}

func (p *payload) normalize() {
	p.N = first(p.N, p.Name)
	p.U = first(p.U, p.URL)
	p.D = first(p.D, p.Domain)
	p.R = first(p.R, p.Referrer)
	if p.W == 0 {
		p.W = p.ScreenWidth
	}
	if len(p.P) == 0 {
		p.P = p.Props
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var p payload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	p.normalize()
	if p.N == "" || p.U == "" {
		http.Error(w, "missing name or url", http.StatusBadRequest)
		return
	}
	pageURL, err := url.Parse(p.U)
	if err != nil || (pageURL.Scheme != "http" && pageURL.Scheme != "https") {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	// From here on the response is always 202, so the endpoint never reveals
	// which domains are registered or why an event was discarded.
	defer w.WriteHeader(http.StatusAccepted)

	ua := r.UserAgent()
	if isBot(r, ua) {
		return
	}

	domains := strings.Split(p.D, ",")
	if p.D == "" {
		domains = []string{pageURL.Hostname()}
	}
	now := time.Now().UTC()
	ip := ClientIP(r, h.TrustProxy || (h.TrustRequest != nil && h.TrustRequest(r)))

	hostname := strings.TrimPrefix(strings.ToLower(pageURL.Hostname()), "www.")
	pathname := pageURL.Path
	if pathname == "" {
		pathname = "/"
	}
	if p.H == 1 && pageURL.Fragment != "" {
		pathname += "#" + pageURL.Fragment
	}
	q := pageURL.Query()
	referrer, source := parseReferrer(p.R, pageURL.Hostname())
	if s := first(q.Get("utm_source"), q.Get("ref"), q.Get("source")); s != "" {
		source = normalizeUTMSource(s)
	}
	loc := h.Geo.Lookup(ip)
	agent := useragent.Parse(ua)

	base := store.Event{
		Timestamp:      now,
		Name:           clip(p.N, 120),
		Hostname:       clip(hostname, 255),
		Pathname:       clip(pathname, 2000),
		Referrer:       clip(referrer, 1000),
		ReferrerSource: clip(source, 255),
		UTMSource:      clip(q.Get("utm_source"), 255),
		UTMMedium:      clip(q.Get("utm_medium"), 255),
		UTMCampaign:    clip(q.Get("utm_campaign"), 255),
		UTMContent:     clip(q.Get("utm_content"), 255),
		UTMTerm:        clip(q.Get("utm_term"), 255),
		Country:        loc.Country,
		Region:         loc.Region,
		City:           loc.City,
		Browser:        agent.Name,
		BrowserVersion: majorVersion(agent.Version),
		OS:             agent.OS,
		OSVersion:      agent.OSVersion,
		Device:         deviceType(p.W, agent),
		ScreenWidth:    uint16(min(max(p.W, 0), 65535)),
		Props:          encodeProps(p.P),
		HasLocation:    loc.HasCoords,
		Latitude:       float32(loc.Lat),
		Longitude:      float32(loc.Lon),
	}

	// Several domains can point to the same site, e.g. its current domain
	// and an old one. Record the event once per site.
	seen := map[int32]bool{}
	for _, d := range domains {
		site, ok := h.Store.SiteByDomain(d)
		if !ok || seen[site.ID] {
			continue
		}
		seen[site.ID] = true
		cur, prev, err := h.Salts.Get(now, site.Timezone)
		if err != nil {
			h.Log.Error("load salt", "err", err)
			return
		}
		e := base
		e.SiteID = site.ID
		visitor := visitorHash(cur, site.ID, ip, ua)
		var prevVisitor uint64
		if prev != nil {
			prevVisitor = visitorHash(prev, site.ID, ip, ua)
		}
		var attr attribution
		e.VisitorID, e.SessionID, attr = h.sessions.touch(visitor, prevVisitor, now, attribution{
			referrer: e.Referrer, source: e.ReferrerSource,
			utmSource: e.UTMSource, utmMedium: e.UTMMedium, utmCampaign: e.UTMCampaign,
			utmContent: e.UTMContent, utmTerm: e.UTMTerm,
		})
		e.Referrer, e.ReferrerSource = attr.referrer, attr.source
		e.UTMSource, e.UTMMedium, e.UTMCampaign = attr.utmSource, attr.utmMedium, attr.utmCampaign
		e.UTMContent, e.UTMTerm = attr.utmContent, attr.utmTerm
		h.Writer.Write(e)
	}
}

// ClientIP returns the request's IP. Proxy headers are only honored when the
// server is configured to sit behind a trusted proxy.
func ClientIP(r *http.Request, trustProxy bool) netip.Addr {
	if trustProxy {
		// X-Client-IP comes first: Amp Boxes set it to the real client IP but pass
		// a visitor's own CF-Connecting-IP through unchanged.
		for _, hdr := range []string{"X-Client-IP", "CF-Connecting-IP", "X-Real-IP", "X-Forwarded-For"} {
			v := r.Header.Get(hdr)
			if v == "" {
				continue
			}
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			if ip, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
				return ip.Unmap()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}

// visitorHash identifies a visitor without storing the IP or setting a cookie.
func visitorHash(salt []byte, siteID int32, ip netip.Addr, ua string) uint64 {
	d := xxhash.New()
	d.Write(salt)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(siteID))
	d.Write(b[:])
	ipb := ip.As16()
	d.Write(ipb[:])
	d.WriteString(ua)
	return d.Sum64()
}

func deviceType(width int, ua useragent.UserAgent) string {
	switch {
	case width <= 0:
		switch {
		case ua.Tablet:
			return "Tablet"
		case ua.Mobile:
			return "Mobile"
		case ua.Desktop:
			return "Desktop"
		}
		return ""
	case width < 576:
		return "Mobile"
	case width < 992:
		return "Tablet"
	case width < 1440:
		return "Laptop"
	default:
		return "Desktop"
	}
}

func majorVersion(v string) string {
	if i := strings.IndexByte(v, '.'); i >= 0 {
		return v[:i]
	}
	return v
}

// encodeProps accepts an object or a JSON-encoded object string and stores
// it as a flat JSON object of strings.
func encodeProps(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return ""
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if len(out) >= 30 {
			break
		}
		if v == nil {
			continue
		}
		var sv string
		switch v := v.(type) {
		case string:
			sv = v
		default:
			b, _ := json.Marshal(v)
			sv = string(b)
		}
		out[clip(k, 64)] = clip(sv, 500)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// clip truncates s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
