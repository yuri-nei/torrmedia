package main

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

var mediaMime = map[string]string{
	".mkv":  "video/x-matroska",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".avi":  "video/x-msvideo",
	".ts":   "video/mp2t",
	".m2ts": "video/mp2t",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".wmv":  "video/x-ms-wmv",
	".mp3":  "audio/mpeg",
	".flac": "audio/flac",
	".aac":  "audio/aac",
	".mka":  "audio/x-matroska",
	".srt":  "application/x-subrip",
	".vtt":  "text/vtt",
	".ass":  "text/x-ssa",
}

func hidden(name string) bool { return strings.HasPrefix(name, ".") }

func complete(info os.FileInfo) bool {
	if info.Size() == 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return st.Blocks*512 >= info.Size()
}

func (s *server) openInRoot(rel string) (*os.File, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}
	root := s.fsRoot()
	if root == nil {
		return nil, fmt.Errorf("media root %q is not available", s.root)
	}
	return root.Open(clean)
}

func (s *server) pruneEmptyDirs(dir string) {
	for strings.HasPrefix(dir, s.root+"/") && filepath.Dir(dir) != s.root {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func availableFilesystemBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func sortStrings(v []string) { sort.Strings(v) }

func validStreamInfo(before, current os.FileInfo) bool {
	return before != nil && current != nil &&
		current.Mode().IsRegular() && os.SameFile(before, current) && complete(current)
}

func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	cat, item, ok := s.resolveItem(w, r)
	if !ok {
		return
	}
	file := item.file(r.PathValue("file"))
	if file == nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "no such file in this item")
		return
	}
	if d := s.permitFile(cat, item, file, actionPlay); d != nil {
		writeDenial(w, d)
		return
	}
	f, err := s.openInRoot(file.shelfRel)
	if err != nil {
		writeErr(w, http.StatusNotFound, codeNotFound, "file is not available: %v", err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !complete(info) {
		writeErr(w, http.StatusNotFound, codeNotFound, "not a complete regular file")
		return
	}
	release, ok := s.streamReg().acquire(inodeKey(info))
	if !ok {
		writeErr(w, http.StatusConflict, codeBusy, "file is being deleted right now")
		return
	}
	defer release()
	current, err := f.Stat()
	if err != nil || !validStreamInfo(info, current) {
		writeErr(w, http.StatusConflict, codeBusy, "file changed while the stream was being opened")
		return
	}
	info = current

	ext := strings.ToLower(filepath.Ext(file.name))
	ct := mediaMime[ext]
	if ct == "" {
		ct = mime.TypeByExtension(ext)
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano()))

	if r.Method != http.MethodGet || s.viewed == nil || !trackableRange(r.Header.Get("Range")) {
		http.ServeContent(w, r, file.name, info.ModTime(), f)
		return
	}
	tracker := newPlaybackTracker(s, file.shelfRel, info.Size(), rangeStart(r.Header.Get("Range")))
	defer tracker.finish()
	cw := &countingWriter{ResponseWriter: w, total: &tracker.total}
	http.ServeContent(cw, r, file.name, info.ModTime(), f)
}
