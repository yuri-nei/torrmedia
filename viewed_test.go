package main

import (
	"path/filepath"
	"testing"
)

// viewedPlayer plays one file of the test item through the same tracker
// entry point the stream handler uses and reads back what the store keeps for
// it.
type viewedPlayer struct {
	env  *testEnv
	rel  string
	size int64
}

// newViewedPlayer scales the absolute playback thresholds down to the test
// env's 1 MiB episodes the way they relate to a real 1 GiB episode: a read
// below minPlayChunk is too short to move the position, and startedBytes
// does not dwarf the whole file.
func newViewedPlayer(t *testing.T) (*viewedPlayer, *catalogItem) {
	t.Helper()
	env := newTestEnv(t)
	env.playback.startedBytes = 16 << 10
	env.playback.minPlayChunk = 8 << 10
	_, item := env.firstItem(t)
	file := item.files[0]
	return &viewedPlayer{env: env, rel: file.shelfRel, size: file.size}, item
}

// play is one request reading n bytes from start, flushed in one go.
func (p *viewedPlayer) play(t *testing.T, start, n int64) {
	t.Helper()
	if err := p.env.trackPlayback(p.rel, p.size, start, n, n); err != nil {
		t.Fatal(err)
	}
}

func (p *viewedPlayer) mark(t *testing.T, state string) {
	t.Helper()
	if err := p.env.markViewed(p.rel, state); err != nil {
		t.Fatal(err)
	}
}

func (p *viewedPlayer) entry() viewedEntry {
	return p.env.viewed.snapshot()[filepath.FromSlash(p.rel)]
}

func (p *viewedPlayer) expect(t *testing.T, step, state string, manual bool) {
	t.Helper()
	entry := p.entry()
	if entry.State != state || entry.Manual != manual {
		t.Fatalf("%s: state=%q manual=%v, want %q manual=%v (served=%d pos=%d size=%d)",
			step, entry.State, entry.Manual, state, manual, entry.Served, entry.Pos, p.size)
	}
}

// resetBytes is how much has to be played after a manual mark before
// automatic tracking takes over again.
func (p *viewedPlayer) resetBytes() int64 {
	return int64(p.env.playback.manualResetFraction*float64(p.size)) + 1
}

// A file the tracker marked watched and the user then marked started used to
// flip back on the very next read: Served still held the whole history, so
// the manual mark was dropped at once and delete_watched took the episode.
func TestManualStartedOutlivesAutoWatched(t *testing.T) {
	p, _ := newViewedPlayer(t)
	p.play(t, 0, p.size)
	p.expect(t, "played through", watchWatched, false)

	p.mark(t, watchStarted)
	p.expect(t, "marked started", watchStarted, true)

	small := p.resetBytes() / 4
	p.play(t, 0, small)
	p.expect(t, "small read after the mark", watchStarted, true)

	// manualResetFraction played after the mark hands the file back to
	// automatic tracking, which counts from the mark: 6% is no reason to
	// call it watched.
	p.play(t, small, p.resetBytes())
	p.expect(t, "reset share played", watchStarted, false)

	// Playing on to the end marks it watched again.
	from := small + p.resetBytes()
	p.play(t, from, p.size-from)
	p.expect(t, "played to the end after the mark", watchWatched, false)
}

// An old seek to the end is what made the tracker call the file watched; once
// the user marked it started, that position must not count again.
func TestManualStartedForgetsOldSeekToTheEnd(t *testing.T) {
	p, item := newViewedPlayer(t)
	chunk := p.size / 16
	p.play(t, p.size-chunk, chunk)
	p.expect(t, "seek to the end", watchWatched, false)

	p.mark(t, watchStarted)

	// The user resumes in the middle and plays enough to end the manual
	// override.
	p.play(t, p.size/2, p.resetBytes())
	p.expect(t, "resumed in the middle", watchStarted, false)
	if pos, want := p.entry().Pos, p.size/2+p.resetBytes(); pos != want {
		t.Fatalf("position=%d, want %d: the old seek is still the resume hint", pos, want)
	}

	// The other episode is really watched, so the selection is not empty for
	// an unrelated reason.
	if err := p.env.markViewed(item.files[1].shelfRel, watchWatched); err != nil {
		t.Fatal(err)
	}
	cat, item := p.env.firstItem(t)
	doomed := p.env.watchedToDelete(cat, item)
	if len(doomed) != 1 || doomed[0].shelfRel != item.files[1].shelfRel {
		names := make([]string, 0, len(doomed))
		for _, file := range doomed {
			names = append(names, file.shelfRel)
		}
		t.Fatalf("delete_watched selects %v, want only %q", names, item.files[1].shelfRel)
	}
}

// Re-reads alone push Served past watchedFraction while the position stays in
// the middle: after a downgrade that position is still the resume hint.
func TestManualStartedKeepsResumePosition(t *testing.T) {
	p, _ := newViewedPlayer(t)
	half := p.size / 2
	p.play(t, 0, half)
	p.play(t, 0, half)
	p.expect(t, "watched by re-reads", watchWatched, false)

	p.mark(t, watchStarted)
	if pos := p.entry().Pos; pos != half {
		t.Fatalf("resume position=%d after the downgrade, want %d", pos, half)
	}
	p.play(t, half, p.resetBytes())
	p.expect(t, "resumed where it stopped", watchStarted, false)
}

// A manual watched mark stays under small reads and is never undone by
// automatic tracking afterwards either.
func TestManualWatchedSurvivesReads(t *testing.T) {
	p, _ := newViewedPlayer(t)
	p.play(t, 0, p.size/4)
	p.expect(t, "a quarter played", watchStarted, false)

	p.mark(t, watchWatched)
	p.expect(t, "marked watched", watchWatched, true)

	small := p.resetBytes() / 4
	p.play(t, 0, small)
	p.expect(t, "small read after the mark", watchWatched, true)

	p.play(t, small, p.resetBytes())
	p.expect(t, "reset share played", watchWatched, false)
}
