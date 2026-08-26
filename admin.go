package main

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
)

// adminRelay is one entry in the admin page's relay switcher. Path is the
// same-origin path the browser POSTs to; ServiceURL is the absolute URL the
// relay expects in the NIP-98 u tag. They are deliberately different, and the
// page must sign ServiceURL rather than anything derived from window.location:
// an owner reaching the relay through an SSH tunnel is browsing
// http://localhost:3355 while ServiceURL still says https://relay.example.com.
type adminRelay struct {
	Key        string
	Label      string
	Path       string
	ServiceURL string
}

type adminPageData struct {
	OwnerPubKey  string
	OwnerNpub    string
	RelayHost    string
	RelayVersion string
	Relays       []adminRelay
	Scripts      []adminScript
}

// adminScript is one of the page's classic scripts, with the cache buster for
// the file currently on disk.
type adminScript struct {
	Src     string
	Version string
}

// adminScripts lists the page's scripts in load order.
//
// They are classic scripts sharing one global scope, not modules, so two of them
// declaring the same top level const would throw and the second would not execute
// at all. Each file therefore prefixes its own bindings, and the order here is
// only about readability — boot() waits for DOMContentLoaded, by which point
// every deferred script has run, so nothing can execute out of order.
func adminScripts() []adminScript {
	paths := []string{
		"/static/admin-charts.js",
		"/static/admin.js",
		"/static/admin-notes.js",
		"/static/admin-dashboard.js",
	}
	out := make([]adminScript, 0, len(paths))
	for _, p := range paths {
		out = append(out, adminScript{Src: p, Version: assetVersion("templates" + p)})
	}
	return out
}

// adminCSP limits what a compromised third party script could do with the
// window.nostr handle this page holds: connect-src 'self' means it cannot send
// anything it signs anywhere, and frame-ancestors blocks clickjacking, which
// matters on a page whose buttons sit one habitual signer approval away from
// taking effect.
//
// media-src is listed because default-src 'none' otherwise blocks every <video>
// and <audio> the media browser tries to play, silently.
//
// 'unsafe-inline' for styles is unavoidable — the Tailwind CDN injects a style
// element at runtime, and the page carries its own inline fallback styles. If
// the CDN ever needs more than this the page renders unstyled, which it is
// built to survive.
const adminCSP = "default-src 'none'; " +
	"script-src 'self' https://cdn.tailwindcss.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src https://fonts.gstatic.com; " +
	"img-src 'self' https: data:; " +
	// blobs are served from this origin and nowhere else, so 'self' is the whole
	// of it. media-src is a real exfiltration channel — new Audio("https://…"+x)
	// is a beacon — and this page holds a live window.nostr handle behind a
	// signer prompt the owner has probably taught themselves to click through,
	// which is exactly what connect-src 'self' is here to contain.
	"media-src 'self'; " +
	"connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// adminTemplate parses the admin page once, lazily, so a missing template file
// is an error on that one page rather than a relay that will not boot.
var adminTemplate = sync.OnceValues(func() (*template.Template, error) {
	return template.ParseFiles("templates/admin.html")
})

// adminHandler serves the relay management UI. The page itself is static and
// gated by nothing: every call it makes carries a NIP-98 event signed in the
// browser, and it holds no relay data until one of those calls comes back.
func adminHandler(w http.ResponseWriter, r *http.Request) {
	if !config.ManagementAPIEnabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tmpl, err := adminTemplate()
	if err != nil {
		slog.Error("🚫 error parsing the admin page", "error", err)
		http.Error(w, "the admin page is unavailable", http.StatusInternalServerError)
		return
	}

	data := adminPageData{
		OwnerPubKey:  config.OwnerPubKey,
		OwnerNpub:    config.OwnerNpub,
		RelayHost:    config.RelayURL,
		RelayVersion: config.RelayVersion,
		Scripts:      adminScripts(),
		// read from the relays themselves rather than rebuilt from config, so
		// the u tag the page signs cannot drift from what the API compares it
		// against if ServiceURL is ever computed differently
		Relays: []adminRelay{
			{relayOutbox, "Outbox", "/", outboxRelay.ServiceURL},
			{relayPrivate, "Private", "/private", privateRelay.ServiceURL},
			{relayChat, "Chat", "/chat", chatRelay.ServiceURL},
			{relayInbox, "Inbox", "/inbox", inboxRelay.ServiceURL},
		},
	}

	var page bytes.Buffer
	if err := tmpl.Execute(&page, data); err != nil {
		slog.Error("🚫 error rendering the admin page", "error", err)
		http.Error(w, "the admin page is unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", adminCSP)

	if _, err := w.Write(page.Bytes()); err != nil {
		slog.Debug("🚫 error writing the admin page", "error", err)
	}
}

// assetVersion busts the browser cache whenever the file on disk changes,
// including between local builds where RelayVersion is always "(devel)".
func assetVersion(path string) string {
	if info, err := os.Stat(path); err == nil {
		return strconv.FormatInt(info.ModTime().Unix(), 10)
	}
	return config.RelayVersion
}
