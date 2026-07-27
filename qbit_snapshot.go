package main

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
		if err != nil {
			return nil, err
		}
		out = append(out, qbTorrentSnapshot{qbTorrent: torrents[i], files: files})
	}
	return out, nil
}
