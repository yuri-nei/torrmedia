package main

// qbit.go is the qBittorrent WebUI API client.
//
// It is the only place in the project that knows about infohashes, file
// indexes and states like stoppedUP. None of this leaks out: the contract with
// the client is described in API.md in its own vocabulary.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// --- qBittorrent WebUI API: a successful login yields a SID cookie (HTTP 204);
// CSRFProtection requires Referer/Origin on every request. ---

type qbit struct {
	base       *url.URL
	user, pass string
	hc         *http.Client
	stateLag   time.Duration // see qbitStateLag
}

// qbitStateLag is how long qBittorrent may keep reporting a torrent's previous
// state in /torrents/info. It caches the state and recomputes it on
// state_update_alert once per Refresh interval (1.5 s by default), and
// commands don't update it. For stop/start this doesn't matter, since there we
// wait for the desired state, but after a recheck a stopped torrent is still
// listed as stoppedUP even though it is already being checked.
const qbitStateLag = 5 * time.Second

type qbTorrent struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	AmountLeft   int64   `json:"amount_left"`
	NumSeeds     int     `json:"num_seeds"`
	DownloadPath string  `json:"download_path"`
	Category     string  `json:"category"`
	Size         int64   `json:"size"`

	// magnet_uri is required: a magnet can't be synthesized from the infohash,
	// as it would be wrong for private and v2-only torrents. The rest are stats
	// for the item header. qBittorrent sends other fields too; we don't decode
	// them because nobody reads them.
	MagnetURI string  `json:"magnet_uri"`
	DlSpeed   int64   `json:"dlspeed"`
	UpSpeed   int64   `json:"upspeed"`
	ETA       int64   `json:"eta"`
	Ratio     float64 `json:"ratio"`
	Completed int64   `json:"completed"`
	NumLeechs int     `json:"num_leechs"`
	AddedOn   int64   `json:"added_on"`

	// Tags arrives as a single comma-separated string. The catalog reads tags
	// only to tell a draft (see drafts.go) from a real item.
	Tags string `json:"tags"`
}

// tagSet parses a qBittorrent tag list: "one, two" -> {"one","two"}.
func (t *qbTorrent) tagSet() map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(t.Tags, ",") {
		if tag := strings.TrimSpace(raw); tag != "" {
			out[tag] = true
		}
	}
	return out
}

func (t *qbTorrent) hasTag(tag string) bool { return t.tagSet()[tag] }

type qbCategory struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

type qbFile struct {
	Index    *int    `json:"index"` // in API >= 2.8.2; otherwise the array position
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Priority int     `json:"priority"`
	Progress float64 `json:"progress"`
}

type qbProperties struct {
	PieceSize int64 `json:"piece_size"`
}

type qbMatch struct {
	hash   string
	fileID int
	abs    string // file path as the torrent sees it (save_path + name)
}

func newQbit(rawURL, user, pass string) (*qbit, error) {
	u, err := url.Parse(strings.TrimSuffix(rawURL, "/"))
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &qbit{
		base: u, user: user, pass: pass, stateLag: qbitStateLag,
		hc: &http.Client{Timeout: 15 * time.Second, Jar: jar},
	}, nil
}

func (q *qbit) do(method, path string, form url.Values) (*http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, q.base.String()+path, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Referer", q.base.String())
	req.Header.Set("Origin", q.base.String())
	return q.hc.Do(req)
}

func (q *qbit) login() error {
	resp, err := q.do("POST", "/api/v2/auth/login", url.Values{"username": {q.user}, "password": {q.pass}})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Success is 200/204 (qBittorrent 5.2.3 answers 204; bad credentials -> 401).
	// The SID cookie, if one arrived, lives in the jar; being authorized is
	// confirmed by the next API request itself (see doAuth), not by inspecting
	// the cookie, because with the localhost bypass qBit issues no SID at all.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("login rejected: HTTP %d — check QBIT_USER/QBIT_PASS", resp.StatusCode)
	}
	return nil
}

// doAuth performs a request with a single re-login on 401/403 (no session or
// an expired SID).
func (q *qbit) doAuth(method, path string, form url.Values) (*http.Response, error) {
	resp, err := q.do(method, path, form)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err := q.login(); err != nil {
			return nil, err
		}
		return q.do(method, path, form)
	}
	return resp, nil
}

func (q *qbit) getJSON(path string, v any) error {
	resp, err := q.doAuth("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// postOK also checks the body: qBittorrent can answer HTTP 200 with the body
// "Fails.", e.g. for a malformed magnet. The body used to be discarded, so
// such a rejection looked like success.
func (q *qbit) postOK(path string, form url.Values) error {
	resp, err := q.doAuth("POST", path, form)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: HTTP %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if text := strings.TrimSpace(string(body)); strings.HasPrefix(text, "Fails") {
		return fmt.Errorf("POST %s: qbittorrent rejected the request (%s)", path, text)
	}
	return nil
}

// getText reads a plain-string response (e.g. /app/version); the health probe
// needs it.
func (q *qbit) getText(path string, out *string) error {
	resp, err := q.doAuth("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	*out = strings.TrimSpace(string(body))
	return nil
}

func (q *qbit) files(hash string) ([]qbFile, error) {
	var files []qbFile
	err := q.getJSON("/api/v2/torrents/files?hash="+url.QueryEscape(hash), &files)
	return files, err
}

// filesIfKnown tells "no metadata yet" apart from a real error. Until the
// magnet resolves, there is no file list, and qBittorrent answers with either
// an empty list or a 404; to us both mean the same "wait some more", not a
// failure.
func (q *qbit) filesIfKnown(hash string) ([]qbFile, bool, error) {
	resp, err := q.doAuth("GET", "/api/v2/torrents/files?hash="+url.QueryEscape(hash), nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict {
		io.Copy(io.Discard, resp.Body)
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GET torrents/files: HTTP %d", resp.StatusCode)
	}
	var files []qbFile
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, false, err
	}
	if len(files) == 0 {
		return nil, false, nil
	}
	return files, true, nil
}

// torrentsWithTag filters the list itself instead of using qBittorrent's tag=
// parameter: there are only dozens of torrents here, and in return the
// behaviour doesn't depend on the WebAPI version.
func (q *qbit) torrentsWithTag(tag string) ([]qbTorrent, error) {
	var torrents []qbTorrent
	if err := q.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		return nil, err
	}
	out := []qbTorrent{}
	for i := range torrents {
		if torrents[i].hasTag(tag) {
			out = append(out, torrents[i])
		}
	}
	return out, nil
}

func (q *qbit) removeTags(hash string, tags []string) error {
	return q.postOK("/api/v2/torrents/removeTags", url.Values{
		"hashes": {hash}, "tags": {strings.Join(tags, ",")},
	})
}

func (q *qbit) setCategory(hash, category string) error {
	return q.postOK("/api/v2/torrents/setCategory", url.Values{
		"hashes": {hash}, "category": {category},
	})
}

func (q *qbit) properties(hash string) (*qbProperties, error) {
	var p qbProperties
	err := q.getJSON("/api/v2/torrents/properties?hash="+url.QueryEscape(hash), &p)
	return &p, err
}

func (q *qbit) torrent(hash string) (*qbTorrent, error) {
	var torrents []qbTorrent
	err := q.getJSON("/api/v2/torrents/info?hashes="+url.QueryEscape(hash), &torrents)
	if err != nil {
		return nil, err
	}
	if len(torrents) == 0 {
		return nil, fmt.Errorf("%w: %q", errTorrentNotFound, hash)
	}
	if len(torrents) != 1 {
		return nil, fmt.Errorf("expected one torrent %q, got %d", hash, len(torrents))
	}
	return &torrents[0], nil
}

func qbitStoppedState(state string) bool {
	return strings.HasPrefix(strings.ToLower(state), "stopped") ||
		strings.HasPrefix(strings.ToLower(state), "paused")
}

func qbitCheckingState(state string) bool {
	return strings.HasPrefix(strings.ToLower(state), "checking")
}

func qbFileByID(files []qbFile, id int) (*qbFile, bool) {
	for i := range files {
		actualID := i
		if files[i].Index != nil {
			actualID = *files[i].Index
		}
		if actualID == id {
			return &files[i], true
		}
	}
	return nil, false
}

func (q *qbit) switchTorrent(hash, action string) error {
	return q.postOK("/api/v2/torrents/"+action, url.Values{"hashes": {hash}})
}

func pollUntil(timeout, interval time.Duration, check func() (bool, error)) error {
	// A non-positive timeout is the caller's exhausted deadline, not "use the
	// default": substituting a default would turn an overrun into a fresh
	// multi-minute wait.
	if timeout <= 0 {
		return errors.New("operation deadline exceeded")
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("operation timed out")
		}
		time.Sleep(interval)
	}
}

func (q *qbit) waitStopped(hash string, timeout, interval time.Duration) error {
	return pollUntil(timeout, interval, func() (bool, error) {
		t, err := q.torrent(hash)
		if err != nil {
			return false, err
		}
		return qbitStoppedState(t.State), nil
	})
}

func (q *qbit) waitStarted(hash string, timeout, interval time.Duration) (*qbTorrent, error) {
	var started *qbTorrent
	err := pollUntil(timeout, interval, func() (bool, error) {
		t, err := q.torrent(hash)
		if err != nil {
			return false, err
		}
		if !qbitStoppedState(t.State) {
			started = t
			return true, nil
		}
		return false, nil
	})
	return started, err
}

func (q *qbit) waitFilePriority(hash string, id, priority int, timeout, interval time.Duration) error {
	return pollUntil(timeout, interval, func() (bool, error) {
		files, err := q.files(hash)
		if err != nil {
			return false, err
		}
		f, ok := qbFileByID(files, id)
		if !ok {
			return false, fmt.Errorf("file id %d disappeared", id)
		}
		return f.Priority == priority, nil
	})
}

// waitChecked waits for the end of a recheck just issued by the recheck
// command and returns the torrent in its final state.
//
// Trap one is the lagging state (see qbitStateLag). Two consecutive polls
// without checking used to be enough here, and they landed on the stale
// stoppedUP: the delete batch "finished" the recheck before it had begun and
// started the torrent mid-check. But qBittorrent itself starts a stopped
// torrent for the duration of the check and stops it again afterwards
// (TorrentImpl::forceRecheck sets StopCondition::FilesChecked, and start
// doesn't clear it), so the enabled episode never downloaded. Hence, until the
// check has been seen, "not checking" during the first stateLag means nothing.
//
// Trap two is duration: a 2160p season takes minutes to check, longer than the
// overall operation timeout. The stall deadline counts from the check's last
// advance (progress while checking is the check's own progress), not from the
// start.
func (q *qbit) waitChecked(hash string, stall, interval time.Duration) (*qbTorrent, error) {
	if stall <= 0 {
		return nil, errors.New("operation deadline exceeded")
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	issued := time.Now()
	moved := issued
	seen := false
	lastProgress := -1.0
	settled := 0
	const requiredSettled = 2 // two polls in a row without checking: surely done
	for {
		t, err := q.torrent(hash)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		switch {
		case qbitCheckingState(t.State):
			seen, settled = true, 0
			if t.Progress != lastProgress {
				lastProgress, moved = t.Progress, now
			}
		case !seen && now.Sub(issued) < q.stateLag:
			// Stale state: the recheck hasn't shown up in the API yet.
		default:
			settled++
			if settled >= requiredSettled {
				return t, nil
			}
		}
		if now.Sub(moved) > stall {
			return nil, fmt.Errorf("recheck made no progress for %s", stall)
		}
		time.Sleep(interval)
	}
}

// waitRecheck waits for a recheck started on a stopped torrent and verifies
// the postcondition: the torrent is stopped again and the file is in place.
func (q *qbit) waitRecheck(hash string, id int, stall, interval time.Duration) error {
	t, err := q.waitChecked(hash, stall, interval)
	if err != nil {
		return err
	}
	if !qbitStoppedState(t.State) {
		return fmt.Errorf("torrent unexpectedly left stopped state after recheck: %s", t.State)
	}
	files, err := q.files(hash)
	if err != nil {
		return err
	}
	if _, ok := qbFileByID(files, id); !ok {
		return fmt.Errorf("file id %d disappeared after recheck", id)
	}
	return nil
}

func (q *qbit) recheck(hash string) error {
	return q.postOK("/api/v2/torrents/recheck", url.Values{"hashes": {hash}})
}

func (q *qbit) setFilePrio(hash string, id, prio int) error {
	return q.postOK("/api/v2/torrents/filePrio", url.Values{
		"hash": {hash}, "id": {strconv.Itoa(id)}, "priority": {strconv.Itoa(prio)},
	})
}

// setFilePrios clears the priority of a whole batch of files in one request:
// when a torrent is added there can be dozens of unselected episodes, and with
// one request per file qBittorrent would rebuild its priority map as many
// times.
func (q *qbit) setFilePrios(hash string, ids []int, prio int) error {
	if len(ids) == 0 {
		return nil
	}
	list := make([]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, strconv.Itoa(id))
	}
	return q.postOK("/api/v2/torrents/filePrio", url.Values{
		"hash": {hash}, "id": {strings.Join(list, "|")}, "priority": {strconv.Itoa(prio)},
	})
}

// waitFilePriorities waits until qBittorrent confirms the priority of all the
// listed files. Confirmation is mandatory: filePrio answers 200 even when the
// priority wasn't applied, and starting a torrent whose priorities weren't
// cleared downloads everything.
func (q *qbit) waitFilePriorities(hash string, ids []int, priority int, timeout, interval time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	return pollUntil(timeout, interval, func() (bool, error) {
		files, err := q.files(hash)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			f, ok := qbFileByID(files, id)
			if !ok {
				return false, fmt.Errorf("file id %d disappeared", id)
			}
			if f.Priority != priority {
				return false, nil
			}
		}
		return true, nil
	})
}

func (q *qbit) deleteTorrent(hash string, deleteFiles bool) error {
	return q.postOK("/api/v2/torrents/delete", url.Values{
		"hashes": {hash}, "deleteFiles": {strconv.FormatBool(deleteFiles)},
	})
}

// --- plumbing ---

// setForceStart is needed only to CLEAR the flag: a torrent marked force start
// ignores the queue and seeding limits, and the flag survives a stop.
func (q *qbit) setForceStart(hash string, value bool) error {
	return q.postOK("/api/v2/torrents/setForceStart", url.Values{
		"hashes": {hash}, "value": {strconv.FormatBool(value)},
	})
}

func (q *qbit) addForm(fields map[string]string) error {
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	return q.postOK("/api/v2/torrents/add", form)
}

// waitCheckDone waits for the end of a check started by the "recheck" button;
// qBittorrent decides what state the torrent ends up in (see waitChecked).
func (q *qbit) waitCheckDone(hash string, stall, interval time.Duration) error {
	_, err := q.waitChecked(hash, stall, interval)
	return err
}
