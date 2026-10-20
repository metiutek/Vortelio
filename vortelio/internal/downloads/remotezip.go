package downloads

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// httpReaderAt reads byte ranges of a remote file, so single members of a big
// zip (e.g. one DLL out of a 700 MB CUDA runtime bundle) can be extracted
// without downloading the whole archive.
type httpReaderAt struct {
	ctx  context.Context
	url  string
	size int64
	// Small reads (archive/zip decompresses through a 4 KB buffer) are served
	// from whole chunks, so a member costs one request per chunk.
	chunks map[int64][]byte
	order  []int64
}

const rzChunk = 8 << 20

func (r *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < r.size {
		idx := off / rzChunk
		c, err := r.chunk(idx)
		if err != nil {
			return n, err
		}
		k := copy(p[n:], c[off-idx*rzChunk:])
		n += k
		off += int64(k)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *httpReaderAt) chunk(idx int64) ([]byte, error) {
	if c, ok := r.chunks[idx]; ok {
		return c, nil
	}
	off := idx * rzChunk
	end := off + rzChunk - 1
	if end >= r.size {
		end = r.size - 1
	}
	var lastErr error
	for try := 0; try < 5; try++ {
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "vortelio")
		req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(end, 10))
		resp, err := client.Do(req)
		if err != nil {
			if r.ctx.Err() != nil {
				return nil, ErrCancelled
			}
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return nil, fmt.Errorf("server does not support ranged reads (HTTP %d)", resp.StatusCode)
		}
		buf := make([]byte, end-off+1)
		_, err = io.ReadFull(resp.Body, buf)
		resp.Body.Close()
		if err != nil {
			if r.ctx.Err() != nil {
				return nil, ErrCancelled
			}
			lastErr = err
			continue
		}
		if r.chunks == nil {
			r.chunks = map[int64][]byte{}
		}
		r.chunks[idx] = buf
		r.order = append(r.order, idx)
		if len(r.order) > 3 { // keep a small window: index + current position
			delete(r.chunks, r.order[0])
			r.order = r.order[1:]
		}
		return buf, nil
	}
	return nil, fmt.Errorf("ranged read failed: %w", lastErr)
}

// remoteSize finds the size of a remote file with a 1-byte ranged GET (HEAD
// is not always honoured by release CDNs).
func remoteSize(ctx context.Context, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "vortelio")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || total <= 0 {
		return 0, errors.New("unknown remote size")
	}
	return total, nil
}

// ZipMember is one file of a remote zip.
type ZipMember struct {
	Name string // path inside the archive
	Size int64  // uncompressed size
	Dest string // where ExtractRemoteZip writes it
}

func openRemoteZip(ctx context.Context, url string) (*zip.Reader, error) {
	size, err := remoteSize(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", filepath.Base(url), err)
	}
	zr, err := zip.NewReader(&httpReaderAt{ctx: ctx, url: url, size: size}, size)
	if err != nil {
		return nil, fmt.Errorf("cannot read archive index: %w", err)
	}
	return zr, nil
}

// RemoteZipListing lists the files of a remote zip and their total size.
func RemoteZipListing(ctx context.Context, url string) ([]ZipMember, int64, error) {
	zr, err := openRemoteZip(ctx, url)
	if err != nil {
		return nil, 0, err
	}
	var out []ZipMember
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		out = append(out, ZipMember{Name: f.Name, Size: int64(f.UncompressedSize64)})
		total += int64(f.UncompressedSize64)
	}
	return out, total, nil
}

// ExtractRemoteZip writes the given members of a remote zip to their Dest,
// reading only the byte ranges they occupy. CRC-32 of each member is checked
// by archive/zip; each file appears only once complete. onBytes reports the
// bytes written so far across all members.
func ExtractRemoteZip(ctx context.Context, url string, members []ZipMember, onBytes func(int64)) error {
	zr, err := openRemoteZip(ctx, url)
	if err != nil {
		return err
	}
	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	var written int64
	buf := make([]byte, 4<<20)
	for _, m := range members {
		f := byName[m.Name]
		if f == nil {
			return fmt.Errorf("%s not found in %s", m.Name, filepath.Base(url))
		}
		if err := os.MkdirAll(filepath.Dir(m.Dest), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		tmp := m.Dest + ".part"
		w, err := os.Create(tmp)
		if err != nil {
			rc.Close()
			return err
		}
		for {
			n, rerr := rc.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					rerr = werr
				}
				written += int64(n)
				if onBytes != nil {
					onBytes(written)
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				w.Close()
				rc.Close()
				os.Remove(tmp)
				if ctx.Err() != nil {
					return ErrCancelled
				}
				return fmt.Errorf("%s: %w", filepath.Base(m.Name), rerr)
			}
		}
		w.Close()
		rc.Close()
		if err := renameRetry(tmp, m.Dest); err != nil {
			return err
		}
	}
	return nil
}

// ExtractRemoteZipMembers copies the members whose base name is in names
// into dir (flat).
func ExtractRemoteZipMembers(ctx context.Context, url string, names []string, dir string, onBytes func(int64)) error {
	list, _, err := RemoteZipListing(ctx, url)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var sel []ZipMember
	for _, m := range list {
		base := filepath.Base(filepath.FromSlash(m.Name))
		if want[base] {
			delete(want, base)
			m.Dest = filepath.Join(dir, base)
			sel = append(sel, m)
		}
	}
	for n := range want {
		return fmt.Errorf("%s not found in %s", n, filepath.Base(url))
	}
	return ExtractRemoteZip(ctx, url, sel, onBytes)
}

// ZipMemberSizes returns the uncompressed size of the members of a remote zip
// whose base name is in names.
func ZipMemberSizes(ctx context.Context, url string, names []string) (int64, error) {
	list, _, err := RemoteZipListing(ctx, url)
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var total int64
	for _, m := range list {
		if want[filepath.Base(filepath.FromSlash(m.Name))] {
			total += m.Size
		}
	}
	return total, nil
}
