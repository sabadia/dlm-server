package bt

import (
	"context"
	"errors"
	"path/filepath"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
)

// errNoStorage is returned by the storage of a torrent that is only being
// resolved: it has no place to put data.
var errNoStorage = errors.New("bt: no storage: the torrent is only being resolved")

// nullStorage is the client's default storage. Torrents that are downloaded
// bring their own; a torrent added only to learn its metadata never writes.
type nullStorage struct{}

func (nullStorage) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	capFunc := func() (int64, bool) { return 0, true }
	return storage.TorrentImpl{
		Piece:    func(metainfo.Piece) storage.PieceImpl { return nullPiece{} },
		Close:    func() error { return nil },
		Capacity: &capFunc,
	}, nil
}

type nullPiece struct{}

func (nullPiece) ReadAt([]byte, int64) (int, error)  { return 0, errNoStorage }
func (nullPiece) WriteAt([]byte, int64) (int, error) { return 0, errNoStorage }
func (nullPiece) MarkComplete() error                { return errNoStorage }
func (nullPiece) MarkNotComplete() error             { return errNoStorage }
func (nullPiece) Completion() storage.Completion {
	return storage.Completion{Ok: true, Complete: false}
}

// fileStorage stores a torrent below the directory of path: a single-file
// torrent is the file path itself, a multi-file torrent the directory path
// with its files inside, whatever the torrent calls itself. Incomplete files
// carry a .part suffix until all their pieces are verified, so a file at its
// final name is a finished file and a restart finds out what is complete
// without hashing it. File names are made valid on every OS.
func fileStorage(path string) storage.ClientImplCloser {
	return storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: filepath.Dir(path),
		FilePathMaker: func(o storage.FilePathMakerOpts) string {
			return relPath(filepath.Base(path), o.Info, o.File)
		},
	})
}

// relPath is where a file of the torrent lies below the task path's
// directory; root is the name of the task path.
func relPath(root string, info *metainfo.Info, f *metainfo.FileInfo) string {
	parts := []string{root}
	if info.IsDir() {
		for _, p := range f.BestPath() {
			parts = append(parts, safeComponent(p))
		}
	}
	return filepath.Join(parts...)
}

// finalPath is the path of a finished file of the torrent stored at path.
func finalPath(path string, info *metainfo.Info, f *metainfo.FileInfo) string {
	return filepath.Join(filepath.Dir(path), relPath(filepath.Base(path), info, f))
}

// safeComponent makes one path component of a torrent usable as a file name
// on every OS ("." and ".." stay out of the result: they would let a torrent
// leave its directory, which the storage refuses anyway).
func safeComponent(s string) string {
	if name := httpx.SanitizeFilename(s); name != "" {
		return name
	}
	return "_"
}

// limitedStorage charges every chunk written to disk against the task's rate
// limiter. A write waits for its budget, which holds back the connection that
// delivered the chunk, so the download slows down to the limit. The limiter
// outlives no download: ctx ends with the run.
type limitedStorage struct {
	storage.ClientImpl
	ctx context.Context //nolint:containedctx // storage callbacks (io.WriterAt) carry no context
	lim engine.Limiter
}

// storageHash is the optional piece hash storage backends are given.
type storageHash = g.Option[[]byte]

func (s limitedStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	t, err := s.ClientImpl.OpenTorrent(ctx, info, ih)
	if err != nil {
		return t, err //nolint:wrapcheck // the storage's own error
	}
	if t.PieceWithHash != nil {
		inner := t.PieceWithHash
		t.PieceWithHash = func(p metainfo.Piece, h storageHash) storage.PieceImpl {
			return limitedPiece{PieceImpl: inner(p, h), ctx: s.ctx, lim: s.lim}
		}
	}
	if t.Piece != nil {
		inner := t.Piece
		t.Piece = func(p metainfo.Piece) storage.PieceImpl {
			return limitedPiece{PieceImpl: inner(p), ctx: s.ctx, lim: s.lim}
		}
	}
	return t, nil
}

type limitedPiece struct {
	storage.PieceImpl
	ctx context.Context //nolint:containedctx // see limitedStorage
	lim engine.Limiter
}

func (p limitedPiece) WriteAt(b []byte, off int64) (int, error) {
	if err := p.lim.WaitN(p.ctx, len(b)); err != nil {
		return 0, err //nolint:wrapcheck // limiter errors are already wrapped
	}
	return p.PieceImpl.WriteAt(b, off) //nolint:wrapcheck // the storage's own error
}
