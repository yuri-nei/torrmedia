package main

// stream.go serves a media file to the player.

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
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

// complete reports whether the actually allocated blocks cover the file's
// WHOLE logical size. qBittorrent writes incomplete data as sparse (holes),
// and with TempPath disabled it writes straight into the library, so this is
// the only way the catalog can tell finished files from downloading ones. The
// threshold is strict: for a fully written file the FS rounds the allocation
// up to a whole block, so blocks*512 >= size always holds; any shortfall
// means holes, i.e. pieces not downloaded yet.
func complete(info os.FileInfo) bool {
	if info.Size() == 0 {
		return false // an empty qBittorrent placeholder; nothing to play
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

// pruneEmptyDirs removes directories left empty, walking up from dir,
// without touching root or the top-level category directories.
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

// totalFilesystemBytes is for display only: "12 of 99 GB free" is readable,
// while "12 GB free" with no scale is not.
func totalFilesystemBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Blocks) * int64(st.Bsize), nil
}

func validStreamInfo(before, current os.FileInfo) bool {
	return before != nil && current != nil &&
		current.Mode().IsRegular() && os.SameFile(before, current) && complete(current)
}

// handleStream serves a file to the player. The server does not parse the
// decorative name in the path: external players need it to key a separate
// playback position on; otherwise all episodes merge into one.
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
	// While the inode is being read it must not be zeroed. Conversely, if it is
	// already claimed for deletion, reading must not start; otherwise the player
	// would get zeros under the Content-Length already sent.
	release, ok := s.streamReg().acquire(inodeKey(info))
	if !ok {
		writeErr(w, http.StatusConflict, codeBusy, "file is being deleted right now")
		return
	}
	defer release()
	// permitFile and the first file check run before acquire. A delete could
	// have gone through entirely in that window and already released its claim,
	// so re-read the metadata after acquiring the inode. While acquire is held,
	// a new delete can no longer change it.
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

	// Multi-range and suffix ranges cannot be tracked: countingWriter also counts
	// the multipart headers, and the start of the first range says nothing about
	// the others, so the viewing progress would be made up.
	if r.Method != http.MethodGet || s.viewed == nil || !trackableRange(r.Header.Get("Range")) {
		http.ServeContent(w, r, file.name, info.ModTime(), f)
		return
	}
	tracker := newPlaybackTracker(s, file.shelfRel, info.Size(), rangeStart(r.Header.Get("Range")))
	defer tracker.finish() // a panic in ServeContent must not leak the goroutine and ticker
	cw := &countingWriter{ResponseWriter: w, total: &tracker.total}
	http.ServeContent(cw, r, file.name, info.ModTime(), f)
}
