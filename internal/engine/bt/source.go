package bt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/settings"
)

// maxTorrentFileBytes bounds a .torrent file.
const maxTorrentFileBytes = 16 << 20

// dataURLPrefix starts the data URL of an embedded .torrent file.
const dataURLPrefix = "data:application/x-bittorrent"

// source is a parsed torrent source: a magnet link (mi is nil until the
// metadata arrives) or the contents of a .torrent file.
type source struct {
	magnet *torrent.TorrentSpec
	mi     *metainfo.MetaInfo
	hash   metainfo.Hash
}

func lowerPrefix(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToLower(s)
}

// match reports whether raw names a torrent source.
func match(raw string) bool {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(lowerPrefix(raw, len("magnet:")), "magnet:"):
		return true
	case strings.HasPrefix(lowerPrefix(raw, len(dataURLPrefix)), dataURLPrefix):
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return (s == "http" || s == "https") && strings.HasSuffix(strings.ToLower(u.Path), ".torrent")
}

// load reads the source. A .torrent URL is fetched with the task's HTTP
// client and settings.
func (e *Engine) load(ctx context.Context, src domain.Source, s settings.Settings) (*source, error) {
	raw := strings.TrimSpace(src.CurrentURL)
	switch {
	case strings.HasPrefix(lowerPrefix(raw, len("magnet:")), "magnet:"):
		return parseMagnet(raw)
	case strings.HasPrefix(lowerPrefix(raw, len(dataURLPrefix)), dataURLPrefix):
		data, err := decodeDataURL(raw)
		if err != nil {
			return nil, engine.Fatal(domain.ErrorCodeFatal, err)
		}
		return parseTorrentFile(data)
	}
	if e.env.Clients == nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, errors.New("bt: no client factory"))
	}
	client, err := e.env.Clients.Client(s)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: client: %w", err))
	}
	pr, err := httpx.Probe(ctx, client, src, s)
	if err != nil {
		return nil, httpx.Classify(err)
	}
	defer func() { _ = pr.Close() }() // the file is read below
	data, err := io.ReadAll(io.LimitReader(pr.Response.Body, maxTorrentFileBytes+1))
	if err != nil {
		return nil, httpx.Classify(fmt.Errorf("bt: read the torrent file: %w", err))
	}
	if len(data) > maxTorrentFileBytes {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: the torrent file is larger than %d bytes", maxTorrentFileBytes))
	}
	return parseTorrentFile(data)
}

func parseMagnet(raw string) (*source, error) {
	spec, err := torrent.TorrentSpecFromMagnetUri(raw)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: invalid magnet link: %v", err))
	}
	var zero metainfo.Hash
	if spec.InfoHash == zero {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: the magnet link has no BitTorrent v1 info hash (v2-only torrents are not supported)"))
	}
	return &source{magnet: spec, hash: spec.InfoHash}, nil
}

func parseTorrentFile(data []byte) (*source, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: not a torrent file: %v", err))
	}
	return fromMetainfo(mi)
}

func fromMetainfo(mi *metainfo.MetaInfo) (*source, error) {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: invalid torrent: %v", err))
	}
	if !info.HasV1() {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: the torrent has no BitTorrent v1 metadata (v2-only torrents are not supported)"))
	}
	if info.TotalLength() < 0 || (info.NumPieces() == 0 && info.TotalLength() > 0) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: invalid torrent: inconsistent sizes"))
	}
	return &source{mi: mi, hash: mi.HashInfoBytes()}, nil
}

// decodeDataURL returns the bytes of a base64 data URL.
func decodeDataURL(raw string) ([]byte, error) {
	head, payload, ok := strings.Cut(raw, ",")
	if !ok || !strings.Contains(strings.ToLower(head), ";base64") {
		return nil, domain.InvalidArgument("bt: the data URL must be base64 encoded (data:application/x-bittorrent;base64,<data>)")
	}
	payload = strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(payload)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err := enc.DecodeString(payload); err == nil {
			if len(data) > maxTorrentFileBytes {
				return nil, fmt.Errorf("bt: the torrent file is larger than %d bytes", maxTorrentFileBytes)
			}
			return data, nil
		}
	}
	return nil, domain.InvalidArgument("bt: the data URL is not valid base64")
}
