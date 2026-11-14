package main

// http_test.go covers the middleware: what wrap refuses before a handler
// runs.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const testMagnetBody = `{"magnet":"magnet:?xt=urn:btih:abc"}`

// newClientRequest builds a request the way a client on the LAN sends it:
// to the server's address, with an explicit content type (empty: none).
func newClientRequest(method, path, contentType, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://192.168.1.10:8091"+path, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func (e *testEnv) send(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.routes().ServeHTTP(rec, req)
	return rec
}

func expectError(t *testing.T, label string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s: status=%d body=%s, want %d", label, rec.Code, rec.Body, status)
	}
	if got := decodeJSON[apiError](t, rec.Body.Bytes()); got.Code != code || got.Error == "" {
		t.Fatalf("%s: error=%+v, want code %q", label, got, code)
	}
}

// expectOperation checks that a mutation was accepted and waits for its
// operation, so nothing runs on after the test.
func (e *testEnv) expectOperation(t *testing.T, rec *httptest.ResponseRecorder) operation {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", rec.Code, rec.Body)
	}
	return e.awaitOperation(t, decodeJSON[operation](t, rec.Body.Bytes()).ID)
}

// A page in the user's browser can POST a text/plain body to any address
// without a CORS preflight. Such a body must not be read as JSON: otherwise
// any web page could add torrents.
func TestRequestBodyMustBeDeclaredAsJSON(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	viewed := "/items/" + item.id + "/files/" + item.files[0].id + "/viewed"

	for _, contentType := range []string{
		"", // none at all
		"text/plain",
		"text/plain;charset=UTF-8",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"application/jsonp",
	} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodPost, "/items", testMagnetBody},
			{http.MethodPost, "/drafts", testMagnetBody},
			{http.MethodPut, viewed, `{"state":"watched"}`},
		} {
			rec := env.send(t, newClientRequest(c.method, c.path, contentType, c.body))
			expectError(t, c.method+" "+c.path+" as "+strconv.Quote(contentType), rec,
				http.StatusUnsupportedMediaType, codeUnsupportedMediaType)
		}
	}
	if calls := env.fake.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("a refused body reached qBittorrent: %v", calls)
	}
	if _, item = env.firstItem(t); item.files[0].watch != "" {
		t.Fatalf("a refused body set the viewed mark: %q", item.files[0].watch)
	}
}

func TestJSONBodyMayCarryParameters(t *testing.T) {
	env := newTestEnv(t)
	for _, contentType := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"Application/JSON;charset=UTF-8",
	} {
		rec := env.send(t, newClientRequest(http.MethodPost, "/items", contentType, testMagnetBody))
		if op := env.expectOperation(t, rec); op.State != opDone {
			t.Fatalf("Content-Type %q: operation=%+v", contentType, op)
		}
	}
	adds := 0
	for _, call := range env.fake.snapshotCalls() {
		if strings.HasPrefix(call, "add:") {
			adds++
		}
	}
	if adds != 3 {
		t.Fatalf("qBittorrent saw %d adds, want 3", adds)
	}
}

// Commands without parameters have no body, so there is no content type to
// demand.
func TestBodylessCommandNeedsNoContentType(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	rec := env.send(t, newClientRequest(http.MethodPost, "/items/"+item.id+"/start", "", ""))
	if op := env.expectOperation(t, rec); op.State != opDone {
		t.Fatalf("operation=%+v", op)
	}
}

// Browsers mark a request made by a page on another site with
// Sec-Fetch-Site: cross-site. No mutation is taken from there, with or
// without a body.
func TestCrossSiteMutationsAreRefused(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	viewed := "/items/" + item.id + "/files/" + item.files[0].id + "/viewed"

	for _, c := range []struct{ method, path, contentType, body string }{
		{http.MethodPost, "/items", "application/json", testMagnetBody},
		{http.MethodPost, "/items/" + item.id + "/stop", "", ""},
		{http.MethodPut, viewed, "application/json", `{"state":"watched"}`},
		{http.MethodDelete, "/items/" + item.id + "?delete_files=true", "", ""},
	} {
		req := newClientRequest(c.method, c.path, c.contentType, c.body)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		expectError(t, c.method+" "+c.path, env.send(t, req), http.StatusForbidden, codeRefused)
	}
	if calls := env.fake.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("a cross-site request reached qBittorrent: %v", calls)
	}
	if _, item = env.firstItem(t); item.files[0].watch != "" {
		t.Fatalf("a cross-site request set the viewed mark: %q", item.files[0].watch)
	}
}

func TestSameSiteAndUnmarkedMutationsAreAccepted(t *testing.T) {
	env := newTestEnv(t)
	for _, site := range []string{"", "same-origin", "same-site", "none"} {
		req := newClientRequest(http.MethodPost, "/items", "application/json", testMagnetBody)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		if op := env.expectOperation(t, env.send(t, req)); op.State != opDone {
			t.Fatalf("Sec-Fetch-Site %q: operation=%+v", site, op)
		}
	}
}

// Reads stay open to other sites: a cross-site page cannot read the
// response, and a link to a stream must keep working.
func TestCrossSiteReadsAreAllowed(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	file := item.files[0]
	stream := "/items/" + item.id + "/files/" + file.id + "/stream/e01.mkv"

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/items"},
		{http.MethodGet, stream},
		{http.MethodHead, stream},
	} {
		req := newClientRequest(c.method, c.path, "", "")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		if rec := env.send(t, req); rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}

// DNS rebinding: a page on the attacker's domain points that domain at this
// server and then talks to it same-origin. Only Host gives it away, so a
// name the server was not told about is refused on every route.
func TestHostMustBeKnown(t *testing.T) {
	env := newTestEnv(t)
	_, item := env.firstItem(t)
	stream := "/items/" + item.id + "/files/" + item.files[0].id + "/stream/e01.mkv"

	for _, host := range []string{
		"192.168.1.10:8091", "192.168.1.10", "[::1]:8091", "[::1]",
		"[fe80::1%25en0]:8091",
		"localhost", "localhost:8091", "LocalHost", "tv.localhost:8091",
		"mac-mini.local", "Mac-Mini.local.:8091",
	} {
		req := newClientRequest(http.MethodGet, "/health", "", "")
		req.Host = host
		if rec := env.send(t, req); rec.Code != http.StatusOK {
			t.Fatalf("Host %q: status=%d body=%s", host, rec.Code, rec.Body)
		}
	}

	for _, host := range []string{
		"evil.example", "evil.example:8091", "EVIL.example.", "example.com",
		"localhost.evil.example", "mac-mini.local.evil.example",
		"192.168.1.10.nip.io", "local", "",
	} {
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/health"},
			{http.MethodGet, "/items"},
			{http.MethodGet, "/events"},
			{http.MethodGet, stream},
			{http.MethodHead, stream},
			{http.MethodDelete, "/items/" + item.id + "?delete_files=true"},
		} {
			req := newClientRequest(c.method, c.path, "", "")
			req.Host = host
			rec := env.send(t, req)
			label := c.method + " " + c.path + " Host " + strconv.Quote(host)
			expectError(t, label, rec, http.StatusMisdirectedRequest, codeMisdirected)
			if msg := decodeJSON[apiError](t, rec.Body.Bytes()).Error; !strings.Contains(msg, "TORRMEDIA_HOSTS") {
				t.Fatalf("%s: the error does not say how to allow the name: %q", label, msg)
			}
		}
	}
	if calls := env.fake.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("a request for a foreign host reached qBittorrent: %v", calls)
	}
}

// The host check comes before the token: a rebound page has no token, and a
// wrong name must be told apart from a wrong token.
func TestHostIsCheckedBeforeToken(t *testing.T) {
	env := newTestEnv(t)
	env.token = "secret"
	req := newClientRequest(http.MethodGet, "/items", "", "")
	req.Host = "evil.example:8091"
	req.Header.Set("Authorization", "Bearer secret")
	expectError(t, "foreign host with token", env.send(t, req), http.StatusMisdirectedRequest, codeMisdirected)
}

func TestHostAllowlistFromEnvironment(t *testing.T) {
	env := newTestEnv(t)
	env.hosts = parseHostAllowlist(" Media.Example.org, ,.TS.net.,, ")

	for host, want := range map[string]int{
		"media.example.org":             http.StatusOK,
		"MEDIA.example.org:8091":        http.StatusOK,
		"media.example.org.":            http.StatusOK,
		"mac-mini.tail1234.ts.net":      http.StatusOK,
		"mac-mini.tail1234.ts.net:8091": http.StatusOK,
		"example.org":                   http.StatusMisdirectedRequest,
		"other.example.org":             http.StatusMisdirectedRequest,
		"media.example.org.evil":        http.StatusMisdirectedRequest,
		"ts.net":                        http.StatusMisdirectedRequest, // the suffix needs a label in front
		"evilts.net":                    http.StatusMisdirectedRequest, // and starts at a dot
		"evil.example":                  http.StatusMisdirectedRequest,
	} {
		req := newClientRequest(http.MethodGet, "/health", "", "")
		req.Host = host
		if rec := env.send(t, req); rec.Code != want {
			t.Fatalf("Host %q: status=%d body=%s, want %d", host, rec.Code, rec.Body, want)
		}
	}
	if got := parseHostAllowlist(" , ,"); got.names != nil || got.suffixes != nil {
		t.Fatalf("empty entries were kept: %+v", got)
	}
}

// Over a real connection the Host header is what the browser sends, not the
// address it connected to: the rebound page reaches this server's IP with
// its own name in Host.
func TestHostCheckOverRealConnection(t *testing.T) {
	env := newTestEnv(t)
	srv := httptest.NewServer(env.routes())
	defer srv.Close()

	for host, want := range map[string]int{
		"":                      http.StatusOK, // the client sends 127.0.0.1:port
		"attacker.example:8091": http.StatusMisdirectedRequest,
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("Host %q: status=%d, want %d", host, resp.StatusCode, want)
		}
	}
}
