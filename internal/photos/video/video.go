// Package video — transcodificación bajo demanda a HLS con ffmpeg.
// Genera la playlist + segmentos en una caché en disco, por etag. La generación
// es síncrona (adecuada para vídeos cortos); para muy largos habría que hacerlo
// en segundo plano.
package video

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gnacho/ocapps/internal/common/webdav"
)

type Transcoder struct {
	dav *webdav.Client
	dir string
}

func New(c *webdav.Client, cacheDir string) (*Transcoder, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	return &Transcoder{dav: c, dir: cacheDir}, nil
}

func key(href, etag string) string {
	h := sha1.Sum([]byte(href + "|" + etag))
	return hex.EncodeToString(h[:])
}

// HLS devuelve el directorio con index.m3u8 + segmentos, generándolo si hace falta.
func (t *Transcoder) HLS(ctx context.Context, href, etag string) (string, error) {
	outDir := filepath.Join(t.dir, key(href, etag))
	if _, err := os.Stat(filepath.Join(outDir, "index.m3u8")); err == nil {
		return outDir, nil
	}

	// descarga a temporal (ffmpeg necesita buscar en el fichero)
	rc, _, err := t.dav.Download(ctx, href)
	if err != nil {
		return "", err
	}
	tmpIn := filepath.Join(t.dir, key(href, etag)+".in")
	f, err := os.Create(tmpIn)
	if err != nil {
		rc.Close()
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(rc, 8<<30)); err != nil {
		f.Close()
		rc.Close()
		os.Remove(tmpIn)
		return "", err
	}
	f.Close()
	rc.Close()
	defer os.Remove(tmpIn)

	// se genera en un dir temporal y se renombra (atómico)
	tmpDir := outDir + ".tmp"
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", tmpIn,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-profile:v", "main",
		"-c:a", "aac", "-b:a", "128k",
		"-f", "hls", "-hls_time", "6", "-hls_playlist_type", "vod",
		"-hls_segment_filename", filepath.Join(tmpDir, "seg%d.ts"),
		filepath.Join(tmpDir, "index.m3u8"))
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("ffmpeg hls: %w (%s)", err, lastLine(out))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "index.m3u8")); err != nil {
		os.RemoveAll(tmpDir)
		return "", errors.New("ffmpeg no generó la playlist")
	}
	_ = os.RemoveAll(outDir)
	if err := os.Rename(tmpDir, outDir); err != nil {
		return "", err
	}
	return outDir, nil
}

func lastLine(b []byte) string {
	s := string(b)
	if i := len(s) - 1; i >= 0 && s[i] == '\n' {
		s = s[:i]
	}
	if i := lastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
