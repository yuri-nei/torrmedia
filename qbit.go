package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type qbit struct {
	base       *url.URL
	user, pass string
	hc         *http.Client
}

type qbTorrent struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	AmountLeft   int64   `json:"amount_left"`
	NumSeeds     int     `json:"num_seeds"`
	NumComplete  int     `json:"num_complete"`
	Availability float64 `json:"availability"`
	DownloadPath string  `json:"download_path"`
	Category     string  `json:"category"`
	Size         int64   `json:"size"`
	ForceStart   bool    `json:"force_start"`

	MagnetURI     string  `json:"magnet_uri"`
	DlSpeed       int64   `json:"dlspeed"`
	UpSpeed       int64   `json:"upspeed"`
	ETA           int64   `json:"eta"`
	Ratio         float64 `json:"ratio"`
	Downloaded    int64   `json:"downloaded"`
	Uploaded      int64   `json:"uploaded"`
	Completed     int64   `json:"completed"`
	NumLeechs     int     `json:"num_leechs"`
	NumIncomplete int     `json:"num_incomplete"`
	AddedOn       int64   `json:"added_on"`
	CompletionOn  int64   `json:"completion_on"`
}

type qbCategory struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

type qbFile struct {
	Index    *int    `json:"index"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Priority int     `json:"priority"`
	Progress float64 `json:"progress"`
}

type qbProperties struct {
	PieceSize int64 `json:"piece_size"`
}

type qbMatch struct {
	hash     string
	fileID   int
	abs      string
	priority int
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
	return &qbit{base: u, user: user, pass: pass, hc: &http.Client{Timeout: 15 * time.Second, Jar: jar}}, nil
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
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("login rejected: HTTP %d — check QBIT_USER/QBIT_PASS", resp.StatusCode)
	}
	return nil
}

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

func (q *qbit) findFile(target os.FileInfo) (*qbMatch, error) {
	var torrents []qbTorrent
	if err := q.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		return nil, err
	}
	var match *qbMatch
	for _, t := range torrents {
		var files []qbFile
		if err := q.getJSON("/api/v2/torrents/files?hash="+url.QueryEscape(t.Hash), &files); err != nil {
			return nil, err
		}
		for i, f := range files {
			abs := filepath.Join(t.SavePath, f.Name)
			fi, err := os.Stat(abs)
			if err != nil {
				continue
			}
			if os.SameFile(target, fi) {
				id := i
				if f.Index != nil {
					id = *f.Index
				}
				if match != nil && (match.hash != t.Hash || match.fileID != id) {
					return nil, errors.New("file inode belongs to multiple torrent entries; refusing ambiguous delete")
				}
				match = &qbMatch{hash: t.Hash, fileID: id, abs: abs, priority: f.Priority}
			}
		}
	}
	return match, nil
}

func (q *qbit) findTorrentByPath(abs string) (*qbTorrent, error) {
	var torrents []qbTorrent
	if err := q.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		return nil, err
	}
	for _, t := range torrents {
		for _, base := range []string{t.ContentPath, filepath.Join(t.SavePath, t.Name)} {
			if base == "" {
				continue
			}
			base = strings.TrimSuffix(base, "/")
			if abs == base || strings.HasPrefix(abs, base+"/") {
				return &t, nil
			}
		}
	}
	return nil, nil
}

func (q *qbit) files(hash string) ([]qbFile, error) {
	var files []qbFile
	err := q.getJSON("/api/v2/torrents/files?hash="+url.QueryEscape(hash), &files)
	return files, err
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
	legacy := map[string]string{"start": "resume", "stop": "pause"}[action]
	err := q.postOK("/api/v2/torrents/"+action, url.Values{"hashes": {hash}})
	if err != nil && legacy != "" {
		err = q.postOK("/api/v2/torrents/"+legacy, url.Values{"hashes": {hash}})
	}
	return err
}

func pollUntil(timeout, interval time.Duration, check func() (bool, error)) error {
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

func (q *qbit) waitRecheck(hash string, id int, timeout, interval time.Duration) error {
	settled := 0
	const requiredSettled = 2
	return pollUntil(timeout, interval, func() (bool, error) {
		t, err := q.torrent(hash)
		if err != nil {
			return false, err
		}
		if qbitCheckingState(t.State) {
			settled = 0
			return false, nil
		}
		if !qbitStoppedState(t.State) {
			return false, fmt.Errorf("torrent unexpectedly left stopped state after recheck: %s", t.State)
		}
		files, err := q.files(hash)
		if err != nil {
			return false, err
		}
		if _, ok := qbFileByID(files, id); !ok {
			return false, fmt.Errorf("file id %d disappeared after recheck", id)
		}
		settled++
		return settled >= requiredSettled, nil
	})
}

func (q *qbit) addTorrentFile(name string, data []byte, fields map[string]string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	part, err := mw.CreateFormFile("torrents", name)
	if err != nil {
		return err
	}
	if _, err = part.Write(data); err != nil {
		return err
	}
	if err = mw.Close(); err != nil {
		return err
	}
	post := func() (*http.Response, error) {
		req, err := http.NewRequest("POST", q.base.String()+"/api/v2/torrents/add", bytes.NewReader(buf.Bytes()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Referer", q.base.String())
		req.Header.Set("Origin", q.base.String())
		return q.hc.Do(req)
	}
	resp, err := post()
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err := q.login(); err != nil {
			return err
		}
		if resp, err = post(); err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("add: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

func (q *qbit) deleteTorrent(hash string, deleteFiles bool) error {
	return q.postOK("/api/v2/torrents/delete", url.Values{
		"hashes": {hash}, "deleteFiles": {strconv.FormatBool(deleteFiles)},
	})
}

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

func (q *qbit) waitCheckDone(hash string, timeout, interval time.Duration) error {
	settled := 0
	return pollUntil(timeout, interval, func() (bool, error) {
		t, err := q.torrent(hash)
		if err != nil {
			return false, err
		}
		if qbitCheckingState(t.State) {
			settled = 0
			return false, nil
		}
		settled++
		return settled >= 2, nil
	})
}
