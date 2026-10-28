package file

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// maxFetchOutputBytes bounds a single artifact pulled from the bucket.
const maxFetchOutputBytes = 2 << 30

// FetchOutput downloads a generated artifact that lives in the tenant bucket
// (relPath is relative to the outputs prefix, e.g. "video/x.mp4") into dst.
// Internal consumers such as the video renderer use it because ffmpeg reads
// files, not bucket URLs. The tenant has no bucket → an error is returned and
// the caller reports the artifact as unavailable.
func (s *Service) FetchOutput(ctx context.Context, relPath, dst string) error {
	target, err := s.OutputTarget(relPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download output: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download output: bucket answered %d", resp.StatusCode)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxFetchOutputBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download output: %w", err)
	}
	if n > maxFetchOutputBytes {
		return fmt.Errorf("download output: file too large")
	}
	return nil
}

// FetchOutput is the Handler bridge used by other modules (render).
func (h *Handler) FetchOutput(ctx context.Context, relPath, dst string) error {
	return h.svc.FetchOutput(ctx, relPath, dst)
}
