package main

import "errors"

// qbitSnapshot captures the torrents and their files in a single pass.
//
// That is N+1 requests to qBittorrent per poller tick, which costs nothing for
// a dozen torrents. If there are ever hundreds, the right next step is not
// caching here but switching to /sync/maindata with incremental updates.
func (s *server) qbitSnapshot() ([]qbTorrentSnapshot, error) {
	if s.qb == nil {
		return nil, nil
	}
	var torrents []qbTorrent
	if err := s.qb.getJSON("/api/v2/torrents/info", &torrents); err != nil {
		return nil, err
	}
	out := make([]qbTorrentSnapshot, 0, len(torrents))
	for i := range torrents {
		files, err := s.qb.files(torrents[i].Hash)
		if errors.Is(err, errTorrentNotFound) {
			// Removed between the two requests (an item removal, a discarded
			// or expired draft). The next tick won't list it at all; failing
			// here would show the whole catalog as qBittorrent being down.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, qbTorrentSnapshot{qbTorrent: torrents[i], files: files})
	}
	return out, nil
}
